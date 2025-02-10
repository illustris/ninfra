package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

type HostConfig struct {
	IP   string `json:"ip"`
	Path string `json:"path"`
}

type Configurations map[string]HostConfig

var (
	deployFilePath = "default.nix"
	skipBuild      = false
	trustNewKeys   = true
	useSubstitutes = true
	verbose        = false
	timeout        time.Duration
)

func parseConfigurations() (Configurations, error) {
	log.Printf("Reading deploy.nix from: %s", deployFilePath)
	data, err := os.ReadFile(deployFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", deployFilePath, err)
	}
	cmd := exec.Command("nix", "eval", "--json", ".#nixosConfigurations", "--apply", string(data))
	output, err := cmd.Output()
	if err != nil {
		log.Printf("Error parsing configurations. Output: %s", string(output))
		return nil, fmt.Errorf("failed to eval configurations: %w", err)
	}
	var configs Configurations
	if err := json.Unmarshal(output, &configs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal configurations: %w", err)
	}
	return configs, nil
}

// sshBaseArgs builds the initial SSH arguments (including the host target)
// and optionally adds a "--" separator if needed.
func sshBaseArgs(username, address string, addSeparator bool) []string {
	sshTgt := fmt.Sprintf("%s@%s", username, address)
	args := []string{}
	if trustNewKeys {
		args = append(args, "-o", "StrictHostKeyChecking=accept-new")
	}
	args = append(args, sshTgt)
	if addSeparator {
		args = append(args, "--")
	}
	return args
}

// ActionFunc is the type for functions that build an exec.Cmd for a given action.
type ActionFunc func(host, address string, args []string, username string) *exec.Cmd

// newCmd is a helper that creates an *exec.Cmd using a timeout context if a timeout is set.
func newCmd(name string, args ...string) *exec.Cmd {
	if timeout > 0 {
		// Create a context with timeout. (Note: In a real program you might want to
		// capture the cancel function and ensure it’s called, but here we simply attach the timeout.)
		ctx, _ := context.WithTimeout(context.Background(), timeout)
		return exec.CommandContext(ctx, name, args...)
	}
	return exec.Command(name, args...)
}

func executeAndStreamOutput(host string, cmd *exec.Cmd) error {
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%s: failed to get stdout pipe: %w", host, err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("%s: failed to get stderr pipe: %w", host, err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: failed to start command: %w", host, err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			log.Printf("%s: %s", host, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			log.Printf("%s: error reading stdout: %v", host, err)
		}
	}()

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			log.Printf("%s (stderr): %s", host, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			log.Printf("%s: error reading stderr: %v", host, err)
		}
	}()

	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s: command finished with error: %w", host, err)
	}

	return nil
}

func executeConcurrently(configs Configurations, args []string, username string, parallelism int, action ActionFunc) error {
	semaphore := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	errChan := make(chan error, len(configs))

	for host, cfg := range configs {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(host string, cfg HostConfig) {
			defer wg.Done()
			defer func() { <-semaphore }()

			cmd := action(host, cfg.IP, args, username)
			if cmd == nil {
				log.Printf("%s: skipping due to nil command", host)
				return
			}

			if err := executeAndStreamOutput(host, cmd); err != nil {
				errChan <- fmt.Errorf("%s: %w", host, err)
			}
		}(host, cfg)
	}

	wg.Wait()
	close(errChan)

	if len(errChan) > 0 {
		for err := range errChan {
			log.Println(err)
		}
		return fmt.Errorf("one or more commands failed")
	}

	return nil
}

func filterConfigurations(configs Configurations, filter string) Configurations {
	filteredConfigs := make(Configurations)
	re, err := regexp.Compile(filter)
	if err != nil {
		log.Fatalf("Error compiling filter regex: %s", err)
	}

	log.Println("----\nSelected hosts:")
	for host, cfg := range configs {
		if re.MatchString(host) {
			filteredConfigs[host] = cfg
			log.Printf("%s -> %+v", host, cfg)
		}
	}
	log.Println("----")

	return filteredConfigs
}

func sshAction(host, address string, args []string, username string) *exec.Cmd {
	baseArgs := sshBaseArgs(username, address, true)
	childArgs := append(baseArgs, args...)
	return newCmd("ssh", childArgs...)
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
	return newCmd("nixos-rebuild", cmdArgs...)
}

func nixosBuildAction(host, address string, args []string, username string) *exec.Cmd {
	hostAttr := fmt.Sprintf(".#%s", host)
	sshTgt := fmt.Sprintf("%s@%s", username, address)
	cmdArgs := []string{"build"}
	if useSubstitutes {
		cmdArgs = append(cmdArgs, "--use-substitutes")
	}
	cmdArgs = append(cmdArgs, "--flake", hostAttr, "--target-host", sshTgt)
	return newCmd("nixos-rebuild", cmdArgs...)
}

func applyWithBuild(configs Configurations, args []string, username string, parallelism int) {
	if skipBuild {
		log.Println("Skipping build phase due to --fast flag.")
	} else {
		log.Println("Starting build phase on all nodes...")
		if err := executeConcurrently(configs, nil, username, parallelism, nixosBuildAction); err != nil {
			log.Println("Build phase failed. Aborting apply phase.")
			os.Exit(1)
		}
	}

	log.Println("Build phase succeeded. Starting apply phase...")
	action := "switch"
	if len(args) > 0 {
		action = args[0]
	}
	finalArgs := []string{action}
	if len(args) > 1 {
		finalArgs = append(finalArgs, args[1:]...)
	}

	if err := executeConcurrently(configs, finalArgs, username, parallelism, nixosRebuildAction); err != nil {
		log.Println("Apply phase failed.")
		os.Exit(1)
	}
}

func compareAction(host, address string, args []string, username string) *exec.Cmd {
	baseArgs := sshBaseArgs(username, address, false)
	childArgs := append(baseArgs, "realpath", "/run/current-system", "/nix/var/nix/profiles/system")
	return newCmd("ssh", childArgs...)
}

type compareResult struct {
	host          string
	expected      string
	currentSystem string
	systemProfile string
	err           error
}

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

			lines := strings.Split(string(output), "\n")
			if len(lines) >= 2 {
				res.currentSystem = strings.TrimSpace(lines[0])
				res.systemProfile = strings.TrimSpace(lines[1])
			} else {
				res.err = fmt.Errorf("unexpected output: %s", string(output))
			}
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
			csStatus, spStatus := "MATCH", "MATCH"
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

func printCompareTable(results []compareResult) int {
	mismatches := 0
	fmt.Printf("%-20s %-7s %-7s %-7s\n", "HOST", "ACTIVE", "BOOT", "PERSISTED")
	for _, res := range results {
		active, boot, persisted := "N", "N", "N"
		if res.currentSystem == res.expected {
			active = "Y"
		}
		if res.systemProfile == res.expected {
			boot = "Y"
		}
		if res.currentSystem == res.systemProfile {
			persisted = "Y"
		}
		if active != "Y" || boot != "Y" {
			mismatches++
		}
		fmt.Printf("%-20s %-7s %-7s %-7s\n", res.host, active, boot, persisted)
	}
	return mismatches
}

func main() {
	// make help text cleaner
	os.Args[0] = "ninfra"

	var filter string = ".*"
	username := os.Getenv("USER")
	parallelism := 1
	subcommand := ""

	flag.StringVar(&filter, "on", filter, "Set the filter to apply")
	flag.StringVar(&username, "l", username, "Specify the username for the target host")
	flag.IntVar(&parallelism, "P", parallelism, "Specify the number of parallel jobs")
	flag.StringVar(&deployFilePath, "d", deployFilePath, "Specify the nix expression file that returns the hosts attrset")
	flag.BoolVar(&skipBuild, "fast", false, "Skip the initial build phase")
	flag.BoolVar(&trustNewKeys, "trust-new-keys", trustNewKeys, "Automatically trust new server keys")
	flag.BoolVar(&useSubstitutes, "use-substitutes", useSubstitutes, "Automatically use substitutes when rebuilding")
	flag.BoolVar(&verbose, "v", verbose, "Enable verbose output for compare subcommand")
	flag.DurationVar(&timeout, "timeout", 0, "Timeout for remote commands (e.g. 5m, 1m)")

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

	// Also set the environment variable used by nixos-rebuild.
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
		log.Fatalf("Error parsing configurations: %v", err)
	}

	filteredConfigs := filterConfigurations(configs, filter)

	switch subcommand {
	case "exec":
		if len(args) < 2 {
			log.Fatal("Error: No command specified for exec.")
		}
		if err := executeConcurrently(filteredConfigs, args[1:], username, parallelism, sshAction); err != nil {
			log.Fatal(err)
		}
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
