package main

import (
	"bufio"
	"bytes"
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

var DeployFilePath = "default.nix"

// Options contains configuration flags and settings.
type Options struct {
	DeployFilePath string
	SkipBuild      bool
	TrustNewKeys   bool
	UseSubstitutes bool
	Verbose        bool
	Timeout        time.Duration
	Username       string
	Parallelism    int
	Filter         string
}

var opts Options

// parseConfigurations reads and evaluates the Nix expression file to get host configurations.
func parseConfigurations(deployFilePath string) (Configurations, error) {
	log.Printf("Reading deploy.nix from: %s", deployFilePath)
	data, err := os.ReadFile(deployFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", deployFilePath, err)
	}

	// Evaluate the nix expression.
	cmd := exec.Command("nix", "eval", "--json", ".#nixosConfigurations", "--apply", string(data))

	// Capture stdout and stderr separately.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		log.Printf("Error parsing configurations. Stdout: %s", stdout.String())
		log.Printf("Error parsing configurations. Stderr: %s", stderr.String())
		return nil, fmt.Errorf("failed to eval configurations: %w", err)
	}

	var configs Configurations
	if err := json.Unmarshal(stdout.Bytes(), &configs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal configurations: %w", err)
	}
	return configs, nil
}

// sshBaseArgs builds the base SSH arguments.
func sshBaseArgs(username, address string, addSeparator bool, trustNewKeys bool) []string {
	sshTarget := fmt.Sprintf("%s@%s", username, address)
	args := []string{}
	if trustNewKeys {
		args = append(args, "-o", "StrictHostKeyChecking=accept-new")
	}
	args = append(args, sshTarget)
	if addSeparator {
		args = append(args, "--")
	}
	return args
}

// ActionFunc defines a function that creates an exec.Cmd for a given host action.
type ActionFunc func(host, address string, args []string, username string) *exec.Cmd

// newCmd creates an exec.Cmd, attaching a timeout if one is set.
func newCmd(name string, args ...string) *exec.Cmd {
	if opts.Timeout > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
		// Note: We do not call cancel() here since the command will run until completion.
		// In a more advanced version, you could manage the cancel function after cmd.Wait().
		_ = cancel
		return exec.CommandContext(ctx, name, args...)
	}
	return exec.Command(name, args...)
}

// executeAndStreamOutput streams stdout and stderr for the given command.
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

// executeConcurrently runs the given action concurrently on all hosts.
func executeConcurrently(configs Configurations, args []string, username string, parallelism int, action ActionFunc) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(configs))
	sem := make(chan struct{}, parallelism)

	for host, cfg := range configs {
		wg.Add(1)
		sem <- struct{}{}
		go func(host string, cfg HostConfig) {
			defer wg.Done()
			defer func() { <-sem }()

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

	var errs []string
	for err := range errChan {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		for _, msg := range errs {
			log.Println(msg)
		}
		return fmt.Errorf("one or more commands failed")
	}
	return nil
}

// filterConfigurations returns a subset of configurations matching the filter regex.
func filterConfigurations(configs Configurations, filter string) (Configurations, error) {
	re, err := regexp.Compile(filter)
	if err != nil {
		return nil, fmt.Errorf("error compiling filter regex: %w", err)
	}

	filtered := make(Configurations)
	log.Println("----\nSelected hosts:")
	for host, cfg := range configs {
		if re.MatchString(host) {
			filtered[host] = cfg
			log.Printf("%s -> %+v", host, cfg)
		}
	}
	log.Println("----")
	return filtered, nil
}

// sshAction builds a command to execute an arbitrary command on the host via SSH.
func sshAction(host, address string, args []string, username string) *exec.Cmd {
	baseArgs := sshBaseArgs(username, address, true, opts.TrustNewKeys)
	childArgs := append(baseArgs, args...)
	return newCmd("ssh", childArgs...)
}

// nixosRebuildAction builds the command for a nixos-rebuild action (apply phase).
func nixosRebuildAction(host, address string, args []string, username string) *exec.Cmd {
	hostAttr := fmt.Sprintf(".#%s", host)
	sshTarget := fmt.Sprintf("%s@%s", username, address)
	if len(args) == 0 {
		args = []string{"switch"}
	}

	// Build command arguments with --use-substitutes if enabled.
	cmdArgs := []string{args[0]}
	if opts.UseSubstitutes {
		cmdArgs = append(cmdArgs, "--use-substitutes")
	}
	cmdArgs = append(cmdArgs, "--flake", hostAttr, "--target-host", sshTarget)
	return newCmd("nixos-rebuild", cmdArgs...)
}

// nixosBuildAction builds the command for the build phase.
func nixosBuildAction(host, address string, _ []string, username string) *exec.Cmd {
	hostAttr := fmt.Sprintf(".#%s", host)
	sshTarget := fmt.Sprintf("%s@%s", username, address)
	cmdArgs := []string{"build"}
	if opts.UseSubstitutes {
		cmdArgs = append(cmdArgs, "--use-substitutes")
	}
	cmdArgs = append(cmdArgs, "--flake", hostAttr, "--target-host", sshTarget)
	return newCmd("nixos-rebuild", cmdArgs...)
}

// applyWithBuild performs the build phase (unless skipped) and then the apply phase.
func applyWithBuild(configs Configurations, args []string, username string, parallelism int) {
	if !opts.SkipBuild {
		log.Println("Starting build phase on all nodes...")
		if err := executeConcurrently(configs, nil, username, parallelism, nixosBuildAction); err != nil {
			log.Println("Build phase failed. Aborting apply phase.")
			os.Exit(1)
		}
	} else {
		log.Println("Skipping build phase due to --fast flag.")
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

// compareAction builds the command used to compare system paths.
func compareAction(host, address string, _ []string, username string) *exec.Cmd {
	baseArgs := sshBaseArgs(username, address, false, opts.TrustNewKeys)
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

// getCompareResults collects the compare results from all hosts.
func getCompareResults(configs Configurations, username string, parallelism int) []compareResult {
	var wg sync.WaitGroup
	resultsChan := make(chan compareResult, len(configs))
	sem := make(chan struct{}, parallelism)

	for host, cfg := range configs {
		wg.Add(1)
		sem <- struct{}{}
		go func(host string, cfg HostConfig) {
			defer wg.Done()
			defer func() { <-sem }()

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

			lines := strings.Split(strings.TrimSpace(string(output)), "\n")
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

// printCompareVerbose outputs a detailed comparison report.
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

// printCompareTable outputs a summary table of the comparison.
func printCompareTable(results []compareResult) int {
	mismatches := 0
	fmt.Printf("%-20s %-7s %-7s %-10s\n", "HOST", "ACTIVE", "BOOT", "PERSISTED")
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
		fmt.Printf("%-20s %-7s %-7s %-10s\n", res.host, active, boot, persisted)
	}
	return mismatches
}

func main() {
	// Adjust the program name for cleaner help text.
	os.Args[0] = "ninfra"

	// Set default values.
	opts = Options{
		DeployFilePath: DeployFilePath,
		SkipBuild:      false,
		TrustNewKeys:   true,
		UseSubstitutes: true,
		Verbose:        false,
		Timeout:        0,
		Username:       os.Getenv("USER"),
		Parallelism:    1,
		Filter:         ".*",
	}

	// Define flags.
	flag.StringVar(&opts.Filter, "on", opts.Filter, "Set the filter to apply")
	flag.StringVar(&opts.Username, "l", opts.Username, "Specify the username for the target host")
	flag.IntVar(&opts.Parallelism, "P", opts.Parallelism, "Specify the number of parallel jobs")
	flag.StringVar(&opts.DeployFilePath, "d", opts.DeployFilePath, "Specify the nix expression file that returns the hosts attrset")
	flag.BoolVar(&opts.SkipBuild, "fast", opts.SkipBuild, "Skip the initial build phase")
	flag.BoolVar(&opts.TrustNewKeys, "trust-new-keys", opts.TrustNewKeys, "Automatically trust new server keys")
	flag.BoolVar(&opts.UseSubstitutes, "use-substitutes", opts.UseSubstitutes, "Automatically use substitutes when rebuilding")
	flag.BoolVar(&opts.Verbose, "v", opts.Verbose, "Enable verbose output for compare subcommand")
	flag.DurationVar(&opts.Timeout, "timeout", opts.Timeout, "Timeout for remote commands (e.g. 5m, 1m)")

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
  For exec:   Any command to execute on the target hosts
  For apply:  An optional positional argument to specify the action (default: switch)
  For compare:No additional arguments required
`)
	}

	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		flag.Usage()
		os.Exit(1)
	}
	subcommand := args[0]

	// Set the environment variable used by nixos-rebuild.
	if opts.TrustNewKeys {
		os.Setenv("NIX_SSHOPTS", "-o StrictHostKeyChecking=accept-new")
	}

	configs, err := parseConfigurations(opts.DeployFilePath)
	if err != nil {
		flag.Usage()
		log.Fatalf("Error parsing configurations: %v", err)
	}

	filteredConfigs, err := filterConfigurations(configs, opts.Filter)
	if err != nil {
		log.Fatalf("Error filtering configurations: %v", err)
	}

	switch subcommand {
	case "exec":
		if len(args) < 2 {
			log.Fatal("Error: No command specified for exec.")
		}
		if err := executeConcurrently(filteredConfigs, args[1:], opts.Username, opts.Parallelism, sshAction); err != nil {
			log.Fatal(err)
		}
	case "apply":
		applyWithBuild(filteredConfigs, args[1:], opts.Username, opts.Parallelism)
	case "compare":
		results := getCompareResults(filteredConfigs, opts.Username, opts.Parallelism)
		var mismatches int
		if opts.Verbose {
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
