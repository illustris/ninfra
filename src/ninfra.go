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

type DeploymentInfo struct {
	TargetHost string `json:"targetHost"`
}

type HostConfig struct {
	Deployment DeploymentInfo `json:"deployment"`
	Path       string         `json:"path,omitempty"`
}

type Configurations map[string]HostConfig

// Default Nix expressions
var (
	// Expression to list all hosts from nixosConfigurations
	DefaultListHostsExpr = "builtins.attrNames"

	// Expression to get deployment info for a specific host
	DefaultDeploymentExpr = "cfg: { targetHost = cfg.config.deployment.targetHost or cfg.config.passthru.deployment.targetHost or null; }"

	// Expression to get system path for a specific host
	DefaultClosurePathExpr = "cfg: cfg.config.system.build.toplevel.outPath"
)

// Options contains configuration flags and settings.
type Options struct {
	ListHostsExpr     string
	DeploymentExpr    string
	ClosurePathExpr   string
	SkipBuild         bool
	TrustNewKeys      bool
	UseSubstitutes    bool
	Verbose           bool
	Timeout           time.Duration
	Username          string
	Parallelism       int
	Filter            string
}

var opts Options

// getHostsList returns a list of all host names in the flake.
func getHostsList() ([]string, error) {
	log.Printf("Getting list of hosts...")

	// Build command to get all hosts by name only (lightweight)
	cmd := exec.Command("nix", "eval", "--json", ".#nixosConfigurations", "--apply", opts.ListHostsExpr)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		log.Printf("Error getting hosts list. Stdout: %s", stdout.String())
		log.Printf("Error getting hosts list. Stderr: %s", stderr.String())
		return nil, fmt.Errorf("failed to get hosts list: %w", err)
	}

	var hosts []string
	if err := json.Unmarshal(stdout.Bytes(), &hosts); err != nil {
		return nil, fmt.Errorf("failed to unmarshal hosts list: %w", err)
	}

	return hosts, nil
}

// getHostDeploymentInfo gets deployment information for a specific host
func getHostDeploymentInfo(host string) (DeploymentInfo, error) {
	var deploymentInfo DeploymentInfo

	cmd := exec.Command("nix", "eval", "--json",
		fmt.Sprintf(".#nixosConfigurations.%s", host),
		"--apply", opts.DeploymentExpr)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return deploymentInfo, fmt.Errorf("error evaluating deployment info for %s: %w (stderr: %s)",
			host, err, stderr.String())
	}

	if err := json.Unmarshal(stdout.Bytes(), &deploymentInfo); err != nil {
		return deploymentInfo, fmt.Errorf("failed to unmarshal deployment info for %s: %w", host, err)
	}

	return deploymentInfo, nil
}

// getHostClosurePath gets the system path for a specific host
func getHostClosurePath(host string) (string, error) {
	var path string

	cmd := exec.Command("nix", "eval", "--json",
		fmt.Sprintf(".#nixosConfigurations.%s", host),
		"--apply", opts.ClosurePathExpr)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("error evaluating closure path for %s: %w (stderr: %s)",
			host, err, stderr.String())
	}

	if err := json.Unmarshal(stdout.Bytes(), &path); err != nil {
		return "", fmt.Errorf("failed to unmarshal closure path for %s: %w", host, err)
	}

	return path, nil
}

// getHostsConfigs returns configurations for all hosts that match the filter.
// Uses parallel evaluation for both deployment info and closure paths.
func getHostsConfigs(filter *regexp.Regexp, needsClosurePath bool) (Configurations, error) {
	log.Printf("Getting detailed host configurations...")

	// First get the list of all hosts (lightweight operation)
	allHosts, err := getHostsList()
	if err != nil {
		return nil, err
	}

	// Filter hosts based on the regex before doing heavy evaluations
	var filteredHosts []string
	for _, host := range allHosts {
		if filter.MatchString(host) {
			filteredHosts = append(filteredHosts, host)
		}
	}

	if len(filteredHosts) == 0 {
		return nil, fmt.Errorf("no hosts matched the filter: %s", filter.String())
	}

	log.Printf("Found %d hosts matching filter", len(filteredHosts))

	// Get deployment info for all hosts (can be done in parallel)
	var wg sync.WaitGroup
	type deploymentResult struct {
		host string
		info DeploymentInfo
		err  error
	}

	// Use a buffered channel to collect results
	deploymentChan := make(chan deploymentResult, len(filteredHosts))
	deploymentSem := make(chan struct{}, opts.Parallelism) // Use configured parallelism

	// Start goroutines to get deployment info
	for _, host := range filteredHosts {
		wg.Add(1)
		deploymentSem <- struct{}{}
		go func(h string) {
			defer wg.Done()
			defer func() { <-deploymentSem }()

			info, err := getHostDeploymentInfo(h)
			deploymentChan <- deploymentResult{h, info, err}
		}(host)
	}

	// Wait for all deployment info goroutines to complete
	go func() {
		wg.Wait()
		close(deploymentChan)
	}()

	// Process deployment results and prepare for closure path evaluation
	configs := make(Configurations)
	validHosts := make([]string, 0, len(filteredHosts))
	hostInfo := make(map[string]DeploymentInfo)

	for result := range deploymentChan {
		if result.err != nil {
			log.Printf("Warning: Error getting deployment info for %s: %v", result.host, result.err)
			continue
		}

		// Skip hosts with no target host
		if result.info.TargetHost == "" {
			log.Printf("Warning: Host %s has no target host, skipping", result.host)
			continue
		}

		// Store deployment info
		hostInfo[result.host] = result.info
		validHosts = append(validHosts, result.host)

		// Initialize host config with deployment info
		configs[result.host] = HostConfig{
			Deployment: result.info,
		}
	}

	// If we don't need closure paths or no valid hosts, we're done
	if !needsClosurePath || len(validHosts) == 0 {
		return configs, nil
	}

	// Now get closure paths for valid hosts in parallel
	log.Printf("Getting closure paths for %d hosts in parallel...", len(validHosts))

	type closurePathResult struct {
		host string
		path string
		err  error
	}

	// Use a buffered channel to collect results
	pathChan := make(chan closurePathResult, len(validHosts))
	pathSem := make(chan struct{}, opts.Parallelism) // Use configured parallelism

	// Reset wait group
	wg = sync.WaitGroup{}

	// Start goroutines to get closure paths
	for _, host := range validHosts {
		wg.Add(1)
		pathSem <- struct{}{}
		go func(h string) {
			defer wg.Done()
			defer func() { <-pathSem }()

			path, err := getHostClosurePath(h)
			pathChan <- closurePathResult{h, path, err}
		}(host)
	}

	// Wait for all closure path goroutines to complete
	go func() {
		wg.Wait()
		close(pathChan)
	}()

	// Process closure path results
	for result := range pathChan {
		if result.err != nil {
			log.Printf("Warning: Error getting closure path for %s: %v", result.host, result.err)
			delete(configs, result.host) // Remove host from configs if we can't get its path
			continue
		}

		// Update config with closure path
		cfg := configs[result.host]
		cfg.Path = result.path
		configs[result.host] = cfg
	}

	return configs, nil
}

// parseConfigurations builds the host configurations based on the filter and whether closure paths are needed.
func parseConfigurations(needsClosurePath bool) (Configurations, error) {
	// Compile the filter regex
	re, err := regexp.Compile(opts.Filter)
	if err != nil {
		return nil, fmt.Errorf("error compiling filter regex: %w", err)
	}

	// Get configurations for matching hosts
	return getHostsConfigs(re, needsClosurePath)
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

			cmd := action(host, cfg.Deployment.TargetHost, args, username)
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

// printSelectedHosts logs the selected hosts and their configurations.
func printSelectedHosts(configs Configurations) {
	log.Println("----\nSelected hosts:")
	for host, cfg := range configs {
		if cfg.Path != "" {
			log.Printf("%s -> targetHost: %s, path: %s", host, cfg.Deployment.TargetHost, cfg.Path)
		} else {
			log.Printf("%s -> targetHost: %s", host, cfg.Deployment.TargetHost)
		}
	}
	log.Println("----")
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
	// For the compare command, we handle SSH errors more explicitly to show the user why connections failed
	sshTarget := fmt.Sprintf("%s@%s", username, address)

	// Build the SSH command with options from the environment plus our basic options
	args := []string{}
	if opts.TrustNewKeys {
		args = append(args, "-o", "StrictHostKeyChecking=accept-new")
	}
	args = append(args, sshTarget, "realpath", "/run/current-system", "/nix/var/nix/profiles/system")

	// Debug log the command only in verbose mode
	if opts.Verbose {
		log.Printf("Compare command for %s: ssh %v", host, args)
	}

	return newCmd("ssh", args...)
}

type compareResult struct {
	host          string
	expected      string  // The expected path from the nix evaluation
	currentSystem string  // Path from /run/current-system
	systemProfile string  // Path from /nix/var/nix/profiles/system
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

			cmd := compareAction(host, cfg.Deployment.TargetHost, nil, username)

			// Get stdout and stderr separately
			stdoutPipe, err := cmd.StdoutPipe()
			if err != nil {
				resultsChan <- compareResult{
					host:     host,
					expected: cfg.Path,
					err:      fmt.Errorf("failed to get stdout pipe: %w", err),
				}
				return
			}

			stderrPipe, err := cmd.StderrPipe()
			if err != nil {
				resultsChan <- compareResult{
					host:     host,
					expected: cfg.Path,
					err:      fmt.Errorf("failed to get stderr pipe: %w", err),
				}
				return
			}

			// Start the command
			if err := cmd.Start(); err != nil {
				resultsChan <- compareResult{
					host:     host,
					expected: cfg.Path,
					err:      fmt.Errorf("failed to start command: %w", err),
				}
				return
			}

			// Read stdout
			var stdout bytes.Buffer
			stdoutScanner := bufio.NewScanner(stdoutPipe)
			for stdoutScanner.Scan() {
				stdout.WriteString(stdoutScanner.Text() + "\n")
			}
			if scanErr := stdoutScanner.Err(); scanErr != nil {
				resultsChan <- compareResult{
					host:     host,
					expected: cfg.Path,
					err:      fmt.Errorf("error reading stdout: %w", scanErr),
				}
				return
			}

			// Read stderr
			var stderr bytes.Buffer
			stderrScanner := bufio.NewScanner(stderrPipe)
			for stderrScanner.Scan() {
				stderr.WriteString(stderrScanner.Text() + "\n")
			}
			if scanErr := stderrScanner.Err(); scanErr != nil {
				resultsChan <- compareResult{
					host:     host,
					expected: cfg.Path,
					err:      fmt.Errorf("error reading stderr: %w", scanErr),
				}
				return
			}

			// Wait for command to finish
			err = cmd.Wait()
			res := compareResult{
				host:     host,
				expected: cfg.Path,
			}

			if err != nil {
				res.err = fmt.Errorf("error executing SSH command: %w, stderr: \n%s", err, stderr.String())
				resultsChan <- res
				return
			}

			// Only process stdout for paths
			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if len(lines) >= 2 {
				res.currentSystem = strings.TrimSpace(lines[0])
				res.systemProfile = strings.TrimSpace(lines[1])
			} else {
				res.err = fmt.Errorf("unexpected output from realpath command: %s", stdout.String())
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
// It no longer returns a count as we use the table's count as the source of truth.
func printCompareVerbose(results []compareResult) {
	fmt.Println("==== Compare Report ====")
	for _, res := range results {
		fmt.Printf("Host: %s\n", res.host)
		if res.err != nil {
			fmt.Printf("  Error: %v\n", res.err)
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
		}
		fmt.Println("----")
	}
}

// printCompareTable outputs a summary table of the comparison.
// Returns (mismatches, sshErrors)
func printCompareTable(results []compareResult) (int, int) {
	mismatches := 0
	sshErrors := 0
	fmt.Printf("%-20s %-7s %-7s %-10s %-30s\n", "HOST", "ACTIVE", "BOOT", "PERSISTED", "STATUS")
	for _, res := range results {
		if res.err != nil {
			sshErrors++
			fmt.Printf("%-20s %-7s %-7s %-10s %-30s\n", res.host, "ERR", "ERR", "ERR", "Connection failed")
			continue
		}

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

		status := "OK"
		if active != "Y" || boot != "Y" {
			status = "Mismatch detected"
			mismatches++
		}

		fmt.Printf("%-20s %-7s %-7s %-10s %-30s\n", res.host, active, boot, persisted, status)
	}
	return mismatches, sshErrors
}

func main() {
	// Adjust the program name for cleaner help text.
	os.Args[0] = "ninfra"

	// Set default values.
	opts = Options{
		ListHostsExpr:   DefaultListHostsExpr,
		DeploymentExpr:  DefaultDeploymentExpr,
		ClosurePathExpr: DefaultClosurePathExpr,
		SkipBuild:       false,
		TrustNewKeys:    true,
		UseSubstitutes:  true,
		Verbose:         false,
		Timeout:         0,
		Username:        os.Getenv("USER"),
		Parallelism:     1,
		Filter:          ".*",
	}

	// Define flags.
	flag.StringVar(&opts.Filter, "on", opts.Filter, "Set the filter to apply")
	flag.StringVar(&opts.Username, "l", opts.Username, "Specify the username for the target host")
	flag.IntVar(&opts.Parallelism, "P", opts.Parallelism, "Specify the number of parallel jobs")
	flag.StringVar(&opts.ListHostsExpr, "list-hosts-expr", opts.ListHostsExpr, "Nix expression to list hosts (applied to .#nixosConfigurations)")
	flag.StringVar(&opts.DeploymentExpr, "deployment-expr", opts.DeploymentExpr, "Nix expression to extract deployment info")
	flag.StringVar(&opts.ClosurePathExpr, "closure-path-expr", opts.ClosurePathExpr, "Nix expression to extract system path")
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

	// Determine if we need closure paths based on the subcommand
	needsClosurePath := false
	if len(args) > 0 && (args[0] == "apply" || args[0] == "compare") {
		needsClosurePath = true
	}

	configs, err := parseConfigurations(needsClosurePath)
	if err != nil {
		log.Fatalf("Error parsing configurations: %v", err)
	}

	printSelectedHosts(configs)

	switch subcommand {
	case "exec":
		if len(args) < 2 {
			log.Fatal("Error: No command specified for exec.")
		}
		if err := executeConcurrently(configs, args[1:], opts.Username, opts.Parallelism, sshAction); err != nil {
			log.Fatal(err)
		}
	case "apply":
		applyWithBuild(configs, args[1:], opts.Username, opts.Parallelism)
	case "compare":
		results := getCompareResults(configs, opts.Username, opts.Parallelism)

		// Always show the comparison table (includes SSH errors as connection failures)
		tableMismatches, sshErrors := printCompareTable(results)

		// If verbose mode is on, also show the detailed report
		if opts.Verbose {
			fmt.Println() // Add spacing between table and verbose output
			printCompareVerbose(results)
		}

		// Print appropriate message based on what failed
		if sshErrors > 0 && tableMismatches > 0 {
			fmt.Printf("\nFound %d host(s) with mismatches and %d host(s) with connection errors.\n", tableMismatches, sshErrors)
			os.Exit(1)
		} else if sshErrors > 0 {
			fmt.Printf("\nFound %d host(s) with connection errors.\n", sshErrors)
			os.Exit(1)
		} else if tableMismatches > 0 {
			fmt.Printf("\nFound %d host(s) with mismatches.\n", tableMismatches)
			os.Exit(1)
		}
	default:
		fmt.Printf("Error: Unknown subcommand '%s'.\n", subcommand)
		flag.Usage()
		os.Exit(1)
	}
}
