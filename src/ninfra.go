package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

type HostConfig struct {
	IP   string `json:"ip"`
	Path string `json:"path"`
}

type Configurations map[string]HostConfig

var deployFilePath = "default.nix"
var skipBuild = false
var trustNewKeys = true
var useSubstitutes = true
var verbose = false

func parseConfigurations() (Configurations, error) {
	fmt.Printf("reading deploy.nix from: %s\n", deployFilePath)
	data, err := ioutil.ReadFile(deployFilePath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("nix", "eval", "--json", ".#nixosConfigurations", "--apply", string(data))
	output, err := cmd.Output()
	if err != nil {
		fmt.Printf("Error parsing configurations. Stdout and Stderr: %s\n", string(output))
		return nil, err
	}
	var configs Configurations
	if err := json.Unmarshal(output, &configs); err != nil {
		return nil, err
	}
	return configs, nil
}

type ActionFunc func(host, address string, args []string, username string) *exec.Cmd

func executeAndStreamOutput(host string, cmd *exec.Cmd) error {
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%s: Failed to get stdout pipe: %w", host, err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("%s: Failed to get stderr pipe: %w", host, err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: Failed to start command: %w", host, err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			fmt.Printf("%s: %s\n", host, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			fmt.Printf("%s: Error reading stdout: %v\n", host, err)
		}
	}()

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			fmt.Printf("%s (stderr): %s\n", host, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			fmt.Printf("%s: Error reading stderr: %v\n", host, err)
		}
	}()

	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s: Command finished with error: %w", host, err)
	}

	return nil
}

func executeConcurrently(configs Configurations, args []string, username string, parallelism int, action ActionFunc) error {
	semaphore := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	errors := make(chan error, len(configs))

	for host, cfg := range configs {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(host string, cfg HostConfig) {
			defer wg.Done()
			defer func() { <-semaphore }()

			cmd := action(host, cfg.IP, args, username)
			if cmd == nil {
				fmt.Printf("%s: Skipping due to nil command\n", host)
				return
			}

			if err := executeAndStreamOutput(host, cmd); err != nil {
				errors <- fmt.Errorf("%s: %w", host, err)
			}
		}(host, cfg)
	}

	wg.Wait()
	close(errors)

	if len(errors) > 0 {
		for err := range errors {
			fmt.Println(err)
		}
		return fmt.Errorf("One or more commands failed")
	}

	return nil
}

func filterConfigurations(configs Configurations, filter string) Configurations {
	filteredConfigs := make(Configurations)
	re, err := regexp.Compile(filter)
	if err != nil {
		fmt.Printf("Error compiling filter regex: %s\n", err)
		os.Exit(1)
	}

	fmt.Printf("----\nselected hosts:\n")
	for host, cfg := range configs {
		if re.MatchString(host) {
			filteredConfigs[host] = cfg
			fmt.Printf("%s -> %+v\n", host, cfg)
		}
	}
	fmt.Printf("----\n")

	return filteredConfigs
}

func sshAction(host, address string, args []string, username string) *exec.Cmd {
	sshTgt := fmt.Sprintf("%s@%s", username, address)
	childArgs := append([]string{sshTgt, "--"}, args...)
	if trustNewKeys {
		childArgs = append([]string{"-o", "StrictHostKeyChecking=accept-new"}, childArgs...)
	}
	return exec.Command("ssh", childArgs...)
}

func nixosRebuildAction(host, address string, args []string, username string) *exec.Cmd {
	hostAttr := fmt.Sprintf(".#%s", host)
	sshTgt := fmt.Sprintf("%s@%s", username, address)
	if len(args) == 0 {
		args = []string{"switch"}
	}

	// Build the command arguments, injecting --use-substitutes if enabled.
	cmdArgs := []string{args[0]}
	if useSubstitutes {
		cmdArgs = append(cmdArgs, "--use-substitutes")
	}
	cmdArgs = append(cmdArgs, "--flake", hostAttr, "--target-host", sshTgt)
	return exec.Command("nixos-rebuild", cmdArgs...)
}

func nixosBuildAction(host, address string, args []string, username string) *exec.Cmd {
	hostAttr := fmt.Sprintf(".#%s", host)
	sshTgt := fmt.Sprintf("%s@%s", username, address)
	cmdArgs := []string{"build"}
	if useSubstitutes {
		cmdArgs = append(cmdArgs, "--use-substitutes")
	}
	cmdArgs = append(cmdArgs, "--flake", hostAttr, "--target-host", sshTgt)
	return exec.Command("nixos-rebuild", cmdArgs...)
}

func applyWithBuild(configs Configurations, args []string, username string, parallelism int) {
	if skipBuild {
		fmt.Println("Skipping build phase due to --fast flag.")
	} else {
		fmt.Println("Starting build phase on all nodes...")
		if err := executeConcurrently(configs, nil, username, parallelism, nixosBuildAction); err != nil {
			fmt.Println("Build phase failed. Aborting apply phase.")
			os.Exit(1)
		}
	}

	fmt.Println("Build phase succeeded. Starting apply phase...")
	action := "switch"
	if len(args) > 0 {
		action = args[0]
	}
	finalArgs := []string{action}
	if len(args) > 1 {
		finalArgs = append(finalArgs, args[1:]...)
	}

	if err := executeConcurrently(configs, finalArgs, username, parallelism, nixosRebuildAction); err != nil {
		fmt.Println("Apply phase failed.")
		os.Exit(1)
	}
}

// compareAction returns an *exec.Cmd that SSHes into the target host and
// runs a command to get the real (symlink-resolved) paths for /run/current-system and /nix/var/nix/profiles/system.
func compareAction(host, address string, args []string, username string) *exec.Cmd {
	sshTgt := fmt.Sprintf("%s@%s", username, address)
	// The remote shell prints the resolved paths.
	childArgs := []string{}
	if trustNewKeys {
		childArgs = append(childArgs, "-o", "StrictHostKeyChecking=accept-new")
	}
	childArgs = append(childArgs, sshTgt, "realpath", "/run/current-system", "/nix/var/nix/profiles/system")
	return exec.Command("ssh", childArgs...)
}

// compareResult holds the comparison result from a host.
type compareResult struct {
	host          string
	expected      string
	currentSystem string
	systemProfile string
	err           error
}

// getCompareResults runs compareAction on each host concurrently, collects the results,
// and returns a slice of compareResult.
func getCompareResults(configs Configurations, username string, parallelism int) []compareResult {
	semaphore := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	resultsChan := make(chan compareResult, len(configs))

	for host, cfg := range configs {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(host string, cfg HostConfig) {
			defer wg.Done()
			defer func() { <-semaphore }()

			cmd := compareAction(host, cfg.IP, nil, username)
			output, err := cmd.CombinedOutput()
			res := compareResult{
				host:     host,
				expected: cfg.Path,
			}
			if err != nil {
				res.err = fmt.Errorf("error executing SSH command: %w, output: %s", err, string(output))
				resultsChan <- res
				return
			}

			outStr := string(output)
			lines := strings.Split(outStr, "\n")
			res.currentSystem = strings.TrimSpace(lines[0])
			res.systemProfile = strings.TrimSpace(lines[1])
			resultsChan <- res
		}(host, cfg)
	}

	wg.Wait()
	close(resultsChan)

	var results []compareResult
	for r := range resultsChan {
		results = append(results, r)
	}
	return results
}

// printCompareVerbose prints a detailed report for each host.
func printCompareVerbose(results []compareResult) int {
	mismatches := 0
	fmt.Println("==== Compare Report ====")
	for _, res := range results {
		fmt.Printf("Host: %s\n", res.host)
		if res.err != nil {
			fmt.Printf("  Error: %v\n", res.err)
			mismatches++
		} else {
			fmt.Printf("  Expected path: %s\n", res.expected)
			csStatus := "MATCH"
			spStatus := "MATCH"
			if res.currentSystem != res.expected {
				csStatus = "MISMATCH"
			}
			if res.systemProfile != res.expected {
				spStatus = "MISMATCH"
			}
			fmt.Printf("  /run/current-system: %s (%s)\n", res.currentSystem, csStatus)
			fmt.Printf("  /nix/var/nix/profiles/system: %s (%s)\n", res.systemProfile, spStatus)
			if csStatus == "MISMATCH" || spStatus == "MISMATCH" {
				mismatches++
			}
		}
		fmt.Println("----")
	}
	return mismatches
}

// printCompareTable prints a summary table. For each host, it shows "Y" if the active (/run/current-system)
// or boot (/nix/var/nix/profiles/system) paths match the expected value, and "N" otherwise.
func printCompareTable(results []compareResult) int {
	mismatches := 0
	// Print header.
	fmt.Printf("%-20s %-7s %-7s %-7s\n", "HOST", "ACTIVE", "BOOT", "PERSISTED")
	for _, res := range results {
		// var active, boot, persisted string
		active := "N"
		boot := "N"
		persisted := "N"
		if res.currentSystem == res.expected {
			active = "Y"
		}
		if res.systemProfile == res.expected {
			boot = "Y"
		}
		if !(res.currentSystem == res.expected && res.systemProfile == res.expected) {
			mismatches++
		}
		if res.currentSystem == res.systemProfile {
			persisted = "Y"
		}
		fmt.Printf("%-20s %-7s %-7s %-7s\n", res.host, active, boot, persisted)
	}
	return mismatches
}

func main() {
	// make help text cleaner
	os.Args[0] = "ninfra"

	filter := ".*"
	username := os.Getenv("USER")
	parallelism := 1
	subcommand := ""

	flag.StringVar(&filter, "on", filter, "Set the filter to apply")
	flag.StringVar(&username, "l", username, "Specify the username for the target host")
	flag.IntVar(&parallelism, "P", parallelism, "Specify the number of parallel jobs")
	flag.StringVar(&deployFilePath, "d", deployFilePath, "Specify the nix expression file that returns the hosts attrset")
	flag.BoolVar(&skipBuild, "fast", false, "Skip the initial build phase")
	flag.BoolVar(&trustNewKeys, "trust-new-keys", true, "Automatically trust new server keys")
	flag.BoolVar(&useSubstitutes, "use-substitutes", useSubstitutes, "Automatically use substitutes when rebuilding")
	flag.BoolVar(&verbose, "v", false, "Enable verbose output for compare subcommand")

	// Customize the help output
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %s <subcommand> [options] [arguments]

Subcommands:
  exec               Execute a command on the specified hosts
  apply              Perform nixos-rebuild on the specified hosts
  compare            Compare resolved system paths with the configured path

Options:
`, os.Args[0])
		flag.PrintDefaults()

		fmt.Fprintln(os.Stderr, `
Arguments:
  For exec: Any command to execute on the target hosts
  For apply: An optional positional argument to specify the action (default: switch).
  For compare: No additional arguments required.
`)
	}

	flag.Parse()

	if trustNewKeys {
		os.Setenv("NIX_SSHOPTS", "-o StrictHostKeyChecking=accept-new")
	}

	args := flag.Args()
	if len(args) > 0 {
		subcommand = args[0]
	}

	configs, err := parseConfigurations()
	if err != nil {
		flag.Usage()
		fmt.Printf("Error parsing configurations: %s\n", err)
		os.Exit(1)
	}

	filteredConfigs := filterConfigurations(configs, filter)

	switch subcommand {
	case "exec":
		if len(args) < 2 {
			fmt.Println("Error: No command specified for exec.")
			os.Exit(1)
		}
		executeConcurrently(filteredConfigs, args[1:], username, parallelism, sshAction)

	case "apply":
		applyWithBuild(filteredConfigs, args[1:], username, parallelism)

	case "compare":
		results := getCompareResults(filteredConfigs, username, parallelism)
		var mismatches int
		if verbose {
			mismatches = printCompareVerbose(results)
		} else {
			mismatches = printCompareTable(results)
		}
		if mismatches > 0 {
			fmt.Printf("\nFound %d host(s) with mismatches/errors.\n", mismatches)
			os.Exit(1)
		}

	default:
		fmt.Printf("Error: Unknown subcommand '%s'.\n", subcommand)
		flag.Usage()
		os.Exit(1)
	}
}
