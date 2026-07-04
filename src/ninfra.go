package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
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

	// Expression to get deployment info for a specific host.
	// 'or' only catches missing attributes; accessing a declared-but-never-
	// defined option (e.g. passthru) throws, so the chain is wrapped in
	// tryEval to cleanly exclude hosts that define no deployment info at all.
	DefaultDeploymentExpr = "cfg: { targetHost = let r = builtins.tryEval (cfg.config.deployment.targetHost or cfg.config.passthru.deployment.targetHost or null); in if r.success then r.value else null; }"

	// Expression to get system path for a specific host
	DefaultClosurePathExpr = "cfg: cfg.config.system.build.toplevel.outPath"
)

// Options contains configuration flags and settings.
type Options struct {
	ListHostsExpr   string
	DeploymentExpr  string
	ClosurePathExpr string
	SkipBuild       bool
	TrustNewKeys    bool
	UseSubstitutes  bool
	Verbose         bool
	Timeout         time.Duration
	ConnectTimeout  int
	Username        string
	Parallelism     int
	Filter          string
}

var opts Options

// isValidUsername checks if a username is safe for SSH usage
func isValidUsername(username string) bool {
	if username == "" || len(username) > 32 {
		return false
	}
	for _, r := range username {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// isValidHostname checks if a hostname/address is safe for SSH usage
func isValidHostname(hostname string) bool {
	if hostname == "" || len(hostname) > 253 {
		return false
	}
	// Allow IPv4, IPv6, and domain names
	// Basic validation - no shell metacharacters
	for _, r := range hostname {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) &&
			r != '-' && r != '.' && r != ':' && r != '[' && r != ']' {
			return false
		}
	}
	return true
}

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

// nixQuoteString renders s as a Nix double-quoted string literal.
func nixQuoteString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "${", `\${`)
	return `"` + s + `"`
}

// hostApplyExpr wraps a per-host expression so it can be applied to the
// whole nixosConfigurations set. Selecting the host with builtins.getAttr
// (instead of interpolating it into the CLI attrpath) works for any
// attribute name, including dotted FQDN-style names; the Nix CLI attrpath
// syntax cannot represent quotes or backslashes at all.
func hostApplyExpr(expr, host string) string {
	return fmt.Sprintf("cfgs: (%s) (builtins.getAttr %s cfgs)", expr, nixQuoteString(host))
}

// getHostDeploymentInfo gets deployment information for a specific host
func getHostDeploymentInfo(host string) (DeploymentInfo, error) {
	var deploymentInfo DeploymentInfo

	cmd := exec.Command("nix", "eval", "--json", ".#nixosConfigurations",
		"--apply", hostApplyExpr(opts.DeploymentExpr, host))

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

	cmd := exec.Command("nix", "eval", "--json", ".#nixosConfigurations",
		"--apply", hostApplyExpr(opts.ClosurePathExpr, host))

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
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Panic in deployment channel closer: %v", r)
			}
		}()
		wg.Wait()
		close(deploymentChan)
	}()

	// Process deployment results and prepare for closure path evaluation
	configs := make(Configurations)
	validHosts := make([]string, 0, len(filteredHosts))
	hostInfo := make(map[string]DeploymentInfo)

	for result := range deploymentChan {
		if result.err != nil {
			log.Printf("Excluding host %s: failed to get deployment info: %v", result.host, result.err)
			continue
		}

		// Skip hosts with no target host
		if result.info.TargetHost == "" {
			log.Printf("Excluding host %s: no target host configured", result.host)
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
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Panic in closure path channel closer: %v", r)
			}
		}()
		wg.Wait()
		close(pathChan)
	}()

	// Process closure path results
	for result := range pathChan {
		if result.err != nil {
			log.Printf("Excluding host %s: failed to get closure path: %v", result.host, result.err)
			delete(configs, result.host)
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
func sshBaseArgs(username, address string, addSeparator bool, trustNewKeys bool) ([]string, error) {
	if !isValidUsername(username) {
		return nil, fmt.Errorf("invalid username: %s", username)
	}
	if !isValidHostname(address) {
		return nil, fmt.Errorf("invalid hostname/address: %s", address)
	}

	sshTarget := fmt.Sprintf("%s@%s", username, address)
	// BatchMode makes authentication failures fail fast instead of hanging
	// on a hidden interactive password prompt.
	args := []string{
		"-o", "BatchMode=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", opts.ConnectTimeout),
	}
	if trustNewKeys {
		args = append(args, "-o", "StrictHostKeyChecking=accept-new")
	}
	args = append(args, sshTarget)
	if addSeparator {
		args = append(args, "--")
	}
	return args, nil
}

// ActionFunc defines a function that creates a CmdWithCancel for a given host action.
type ActionFunc func(host, address string, args []string, username string) (*CmdWithCancel, error)

// CmdWithCancel wraps an exec.Cmd and its context cancel function.
type CmdWithCancel struct {
	*exec.Cmd
	Cancel context.CancelFunc
}

// newCmd creates an exec.Cmd, attaching a timeout if one is set.
// Returns a CmdWithCancel that includes the cancel function for proper cleanup.
func newCmd(name string, args ...string) *CmdWithCancel {
	var cmd *exec.Cmd
	cancel := context.CancelFunc(func() {}) // No-op cancel for non-timeout commands
	if opts.Timeout > 0 {
		var ctx context.Context
		ctx, cancel = context.WithTimeout(context.Background(), opts.Timeout)
		cmd = exec.CommandContext(ctx, name, args...)
	} else {
		cmd = exec.Command(name, args...)
	}
	// Bound the time Wait blocks on stdio after the child exits or the
	// timeout fires; otherwise descendants (e.g. ssh spawned by
	// nixos-rebuild) inheriting the pipe write ends can hang the run forever.
	cmd.WaitDelay = 10 * time.Second
	return &CmdWithCancel{Cmd: cmd, Cancel: cancel}
}

// lineWriter is an io.Writer that logs each complete line with a prefix.
// Unlike bufio.Scanner it has no maximum line length; a partial trailing
// line is emitted by Flush.
type lineWriter struct {
	prefix string
	buf    bytes.Buffer
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := w.buf.Next(i + 1)
		log.Printf("%s%s", w.prefix, bytes.TrimRight(line, "\r\n"))
	}
	// Bound memory when a stream never emits newlines (e.g. binary output):
	// flush oversized partial lines in chunks instead of buffering forever.
	if w.buf.Len() > lineWriterFlushLimit {
		w.Flush()
	}
	return len(p), nil
}

// lineWriterFlushLimit caps how much of a newline-free stream lineWriter
// buffers before flushing it as a partial line.
const lineWriterFlushLimit = 1 << 20

func (w *lineWriter) Flush() {
	if w.buf.Len() > 0 {
		log.Printf("%s%s", w.prefix, bytes.TrimRight(w.buf.Bytes(), "\r\n"))
		w.buf.Reset()
	}
}

// executeAndStreamOutput streams stdout and stderr for the given command.
// The exec package drains both streams concurrently, so combined with the
// WaitDelay set in newCmd a full pipe buffer cannot deadlock the run.
func executeAndStreamOutput(host string, cmdWithCancel *CmdWithCancel) error {
	defer cmdWithCancel.Cancel() // Ensure context is properly cleaned up
	cmd := cmdWithCancel.Cmd
	stdout := &lineWriter{prefix: host + ": "}
	stderr := &lineWriter{prefix: host + " (stderr): "}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	stdout.Flush()
	stderr.Flush()
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command exited successfully but something (e.g. an ssh
		// descendant) kept its stdio open past WaitDelay.
		log.Printf("%s: command exited but left stdio open; trailing output may be lost", host)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: command finished with error: %w", host, err)
	}
	return nil
}

// executeConcurrently runs the given action concurrently on all hosts.
func executeConcurrently(configs Configurations, args []string, username string, parallelism int, action ActionFunc) error {
	var wg sync.WaitGroup
	var errorCount int32
	var errorMutex sync.Mutex
	sem := make(chan struct{}, parallelism)

	for host, cfg := range configs {
		wg.Add(1)
		sem <- struct{}{}
		go func(host string, cfg HostConfig) {
			defer wg.Done()
			defer func() { <-sem }()

			cmd, err := action(host, cfg.Deployment.TargetHost, args, username)
			if err != nil {
				log.Printf("Error creating command for %s: %v", host, err)
				errorMutex.Lock()
				errorCount++
				errorMutex.Unlock()
				return
			}
			if cmd == nil {
				log.Printf("%s: skipping due to nil command", host)
				return
			}

			if err := executeAndStreamOutput(host, cmd); err != nil {
				log.Printf("Error executing command for %s: %v", host, err)
				errorMutex.Lock()
				errorCount++
				errorMutex.Unlock()
			}
		}(host, cfg)
	}

	wg.Wait()

	if errorCount > 0 {
		return fmt.Errorf("%d command(s) failed", errorCount)
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
func sshAction(host, address string, args []string, username string) (*CmdWithCancel, error) {
	baseArgs, err := sshBaseArgs(username, address, true, opts.TrustNewKeys)
	if err != nil {
		return nil, err
	}
	childArgs := append(baseArgs, args...)
	return newCmd("ssh", childArgs...), nil
}

// nixosRebuildArgs builds the argument list for a nixos-rebuild invocation.
// The host attribute is passed unquoted: nixos-rebuild quotes the flake
// attribute itself, so pre-quoting here would break it.
// extraArgs are passed through to nixos-rebuild verbatim.
func nixosRebuildArgs(host, address, username, action string, extraArgs []string) ([]string, error) {
	if !isValidUsername(username) {
		return nil, fmt.Errorf("invalid username: %s", username)
	}
	if !isValidHostname(address) {
		return nil, fmt.Errorf("invalid hostname/address: %s", address)
	}
	hostAttr := fmt.Sprintf(".#%s", host)
	sshTarget := fmt.Sprintf("%s@%s", username, address)

	cmdArgs := []string{action}
	if opts.UseSubstitutes {
		cmdArgs = append(cmdArgs, "--use-substitutes")
	}
	cmdArgs = append(cmdArgs, "--flake", hostAttr, "--target-host", sshTarget)
	cmdArgs = append(cmdArgs, extraArgs...)
	return cmdArgs, nil
}

// nixosRebuildAction builds the command for a nixos-rebuild action (apply phase).
func nixosRebuildAction(host, address string, args []string, username string) (*CmdWithCancel, error) {
	if len(args) == 0 {
		args = []string{"switch"}
	}
	cmdArgs, err := nixosRebuildArgs(host, address, username, args[0], args[1:])
	if err != nil {
		return nil, err
	}
	return newCmd("nixos-rebuild", cmdArgs...), nil
}

// nixosBuildAction builds the command for the build phase. args are extra
// user-supplied arguments passed through to nixos-rebuild, same as in the
// apply phase.
func nixosBuildAction(host, address string, args []string, username string) (*CmdWithCancel, error) {
	cmdArgs, err := nixosRebuildArgs(host, address, username, "build", args)
	if err != nil {
		return nil, err
	}
	return newCmd("nixos-rebuild", cmdArgs...), nil
}

// applyWithBuild performs the build phase (unless skipped) and then the apply phase.
// Extra arguments after the action are passed through to nixos-rebuild in
// both phases.
func applyWithBuild(configs Configurations, args []string, username string, parallelism int) {
	action := "switch"
	if len(args) > 0 {
		action = args[0]
	}
	var extraArgs []string
	if len(args) > 1 {
		extraArgs = args[1:]
	}

	if !opts.SkipBuild {
		log.Println("Starting build phase on all nodes...")
		if err := executeConcurrently(configs, extraArgs, username, parallelism, nixosBuildAction); err != nil {
			log.Println("Build phase failed. Aborting apply phase.")
			os.Exit(1)
		}
		log.Println("Build phase succeeded.")
	} else {
		log.Println("Skipping build phase.")
	}

	log.Println("Starting apply phase...")
	finalArgs := append([]string{action}, extraArgs...)

	if err := executeConcurrently(configs, finalArgs, username, parallelism, nixosRebuildAction); err != nil {
		log.Println("Apply phase failed.")
		os.Exit(1)
	}
}

// compareAction builds the command used to compare system paths.
func compareAction(host, address string, _ []string, username string) (*CmdWithCancel, error) {
	baseArgs, err := sshBaseArgs(username, address, true, opts.TrustNewKeys)
	if err != nil {
		return nil, err
	}
	args := append(baseArgs, "realpath", "/run/current-system", "/nix/var/nix/profiles/system")

	// Debug log the command only in verbose mode
	if opts.Verbose {
		log.Printf("Compare command for %s: ssh %v", host, args)
	}

	return newCmd("ssh", args...), nil
}

type compareResult struct {
	host          string
	expected      string // The expected path from the nix evaluation
	currentSystem string // Path from /run/current-system
	systemProfile string // Path from /nix/var/nix/profiles/system
	connErr       bool   // err is an SSH transport/connection failure rather than a remote one
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

			res := compareResult{
				host:     host,
				expected: cfg.Path,
			}

			cmdWithCancel, err := compareAction(host, cfg.Deployment.TargetHost, nil, username)
			if err != nil {
				res.connErr = true
				res.err = fmt.Errorf("failed to create command: %w", err)
				resultsChan <- res
				return
			}
			defer cmdWithCancel.Cancel() // Ensure context cleanup
			cmd := cmdWithCancel.Cmd

			// Capture stdout and stderr separately; the exec package drains
			// both streams concurrently, so a large stderr cannot deadlock
			// the run the way sequential pipe reads could.
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			err = cmd.Run()
			if errors.Is(err, exec.ErrWaitDelay) {
				// Successful exit, but stdio was held open past WaitDelay.
				err = nil
			}
			if err != nil {
				// ssh exits 255 on connection/authentication failure; any
				// other positive exit code comes from the remote command.
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 && exitErr.ExitCode() != 255 {
					res.err = fmt.Errorf("remote command failed: %w, stderr: \n%s", err, stderr.String())
				} else {
					res.connErr = true
					res.err = fmt.Errorf("error executing SSH command: %w, stderr: \n%s", err, stderr.String())
				}
				resultsChan <- res
				return
			}

			// Only process stdout for paths
			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if len(lines) >= 2 {
				res.currentSystem = strings.TrimSpace(lines[0])
				res.systemProfile = strings.TrimSpace(lines[1])
			} else {
				res.err = fmt.Errorf("realpath command didn't return the expected two path lines, got: %s", stdout.String())
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
// Returns (mismatches, connErrors, remoteErrors)
func printCompareTable(results []compareResult) (int, int, int) {
	mismatches := 0
	connErrors := 0
	remoteErrors := 0
	fmt.Printf("%-20s %-7s %-7s %-10s %-30s\n", "HOST", "ACTIVE", "BOOT", "PERSISTED", "STATUS")
	for _, res := range results {
		if res.err != nil {
			status := "Remote error"
			if res.connErr {
				status = "Connection failed"
				connErrors++
			} else {
				remoteErrors++
			}
			fmt.Printf("%-20s %-7s %-7s %-10s %-30s\n", res.host, "ERR", "ERR", "ERR", status)
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
	return mismatches, connErrors, remoteErrors
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
		ConnectTimeout:  10,
		Username:        os.Getenv("USER"),
		Parallelism:     1,
		Filter:          ".*",
	}

	// Define flags.
	flag.StringVar(&opts.Filter, "on", opts.Filter, "Set the filter to apply")
	flag.StringVar(&opts.Username, "l", opts.Username, "Specify the username for the target host")
	flag.IntVar(&opts.Parallelism, "P", opts.Parallelism, "Specify the number of parallel jobs")
	flag.IntVar(&opts.ConnectTimeout, "c", opts.ConnectTimeout, "SSH connection timeout in seconds")
	flag.StringVar(&opts.ListHostsExpr, "list-hosts-expr", opts.ListHostsExpr, "Nix expression to list hosts (applied to .#nixosConfigurations)")
	flag.StringVar(&opts.DeploymentExpr, "deployment-expr", opts.DeploymentExpr, "Nix expression to extract deployment info")
	flag.StringVar(&opts.ClosurePathExpr, "closure-path-expr", opts.ClosurePathExpr, "Nix expression to extract system path")
	flag.BoolVar(&opts.SkipBuild, "fast", opts.SkipBuild, "Skip the initial build phase")
	flag.BoolVar(&opts.TrustNewKeys, "trust-new-keys", opts.TrustNewKeys, "Automatically trust new server keys")
	flag.BoolVar(&opts.UseSubstitutes, "use-substitutes", opts.UseSubstitutes, "Automatically use substitutes when rebuilding")
	flag.BoolVar(&opts.Verbose, "v", opts.Verbose, "Enable verbose output for compare subcommand")
	flag.DurationVar(&opts.Timeout, "timeout", opts.Timeout, "Timeout for remote commands (e.g. 5m, 1m)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %s [options] <subcommand> [options] [arguments]

Subcommands:
  exec               Execute a command on the specified hosts
  apply              Perform nixos-rebuild on the specified hosts
  compare            Compare resolved system paths with the configured path

Options may be given before or after the subcommand. Option parsing stops at
the first non-option argument, so for exec, options must come before the
command to execute.

Options:
`, os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, `
Arguments:
  For exec:    Any command to execute on the target hosts
  For apply:   An optional action (default: switch). Extra arguments after
               an explicitly named action are passed through to nixos-rebuild
  For compare: No additional arguments required`)
	}

	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		flag.Usage()
		os.Exit(1)
	}
	subcommand := args[0]

	// Parse flags again after the subcommand so that both
	// 'ninfra -l root apply' and 'ninfra apply -l root' work. Parsing stops
	// at the first non-flag argument, so an exec command line and extra
	// apply arguments are left untouched.
	if err := flag.CommandLine.Parse(args[1:]); err != nil {
		os.Exit(2)
	}
	args = flag.CommandLine.Args()

	// Validate parallelism parameter
	if opts.Parallelism <= 0 {
		log.Fatalf("Parallelism must be greater than 0, got: %d", opts.Parallelism)
	}

	// Build NIX_SSHOPTS for nixos-rebuild's SSH connections, preserving
	// anything the user already set. User-supplied options come first, and
	// ssh honors the first occurrence of an option, so they take precedence.
	sshOpts := []string{
		"-o BatchMode=yes",
		fmt.Sprintf("-o ConnectTimeout=%d", opts.ConnectTimeout),
	}
	if opts.TrustNewKeys {
		sshOpts = append(sshOpts, "-o StrictHostKeyChecking=accept-new")
	}
	if existing := os.Getenv("NIX_SSHOPTS"); existing != "" {
		sshOpts = append([]string{existing}, sshOpts...)
	}
	os.Setenv("NIX_SSHOPTS", strings.Join(sshOpts, " "))

	// Determine if we need closure paths based on the subcommand
	needsClosurePath := subcommand == "apply" || subcommand == "compare"

	configs, err := parseConfigurations(needsClosurePath)
	if err != nil {
		log.Fatalf("Error parsing configurations: %v", err)
	}
	if len(configs) == 0 {
		log.Fatalf("Error: no deployable hosts: all hosts matching --on %q were excluded (see messages above)", opts.Filter)
	}

	printSelectedHosts(configs)

	switch subcommand {
	case "exec":
		if len(args) < 1 {
			log.Fatal("Error: No command specified for exec.")
		}
		if err := executeConcurrently(configs, args, opts.Username, opts.Parallelism, sshAction); err != nil {
			log.Fatal(err)
		}
	case "apply":
		if len(configs) == 1 && !opts.SkipBuild {
			log.Println("Single target node, implicitly skipping separate build phase.")
			opts.SkipBuild = true
		}
		applyWithBuild(configs, args, opts.Username, opts.Parallelism)
	case "compare":
		results := getCompareResults(configs, opts.Username, opts.Parallelism)

		// Always show the comparison table (includes errors per host)
		tableMismatches, connErrors, remoteErrors := printCompareTable(results)

		// If verbose mode is on, also show the detailed report
		if opts.Verbose {
			fmt.Println() // Add spacing between table and verbose output
			printCompareVerbose(results)
		}

		// Print appropriate message based on what failed
		var problems []string
		if tableMismatches > 0 {
			problems = append(problems, fmt.Sprintf("%d host(s) with mismatches", tableMismatches))
		}
		if connErrors > 0 {
			problems = append(problems, fmt.Sprintf("%d host(s) with connection errors", connErrors))
		}
		if remoteErrors > 0 {
			problems = append(problems, fmt.Sprintf("%d host(s) with remote errors", remoteErrors))
		}
		if len(problems) > 0 {
			fmt.Printf("\nFound %s.\n", strings.Join(problems, ", "))
			os.Exit(1)
		}
	default:
		fmt.Printf("Error: Unknown subcommand '%s'.\n", subcommand)
		flag.Usage()
		os.Exit(1)
	}
}
