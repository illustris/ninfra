package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"bufio"
	"regexp"
	"sync"
)

type Configurations map[string]string

var deployFilePath = "default.nix"
var skipBuild = false
var trustNewKeys = true

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

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: Failed to start command: %w", host, err)
	}

	scanner := bufio.NewScanner(stdoutPipe)
	for scanner.Scan() {
		fmt.Printf("%s: %s\n", host, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: Error reading output: %w", host, err)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s: Command finished with error: %w", host, err)
	}

	return nil
}

func executeConcurrently(configs map[string]string, args []string, username string, parallelism int, action ActionFunc) error {
	semaphore := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	errors := make(chan error, len(configs))

	for host, address := range configs {
		wg.Add(1)
		semaphore <- struct{}{} // Acquire a slot

		go func(host, address string) {
			defer wg.Done()
			defer func() { <-semaphore }() // Release the slot

			cmd := action(host, address, args, username)
			if cmd == nil {
				fmt.Printf("%s: Skipping due to nil command\n", host)
				return
			}

			if err := executeAndStreamOutput(host, cmd); err != nil {
				errors <- fmt.Errorf("%s: %w", host, err)
			}
		}(host, address)
	}

	wg.Wait() // Wait for all goroutines to finish
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

	for host, address := range configs {
		if re.MatchString(host) {
			filteredConfigs[host] = address
		}
	}

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
		args = []string{"switch"} // Default action
	}
	return exec.Command("nixos-rebuild", args[0], "--flake", hostAttr, "--target-host", sshTgt)
}

func nixosBuildAction(host, address string, args []string, username string) *exec.Cmd {
	hostAttr := fmt.Sprintf(".#%s", host)
	sshTgt := fmt.Sprintf("%s@%s", username, address)
	return exec.Command("nixos-rebuild", "build", "--flake", hostAttr, "--target-host", sshTgt)
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

	// Customize the help output
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %s <subcommand> [options] [arguments]

Subcommands:
  exec               Execute a command on the specified hosts
  apply              Perform nixos-rebuild on the specified hosts

Options:
`, os.Args[0])
		flag.PrintDefaults() // Prints all defined flags with their descriptions

		fmt.Fprintln(os.Stderr, `
Arguments:
  For exec: Any command to execute on the target hosts
  For apply: An optional positional argument to specify the action (default: switch).
`)
	}

	flag.Parse()

	// Set environment variable for SSH options if trustNewKeys is enabled
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

	default:
		fmt.Printf("Error: Unknown subcommand '%s'.\n", subcommand)
		flag.Usage()
		os.Exit(1)
	}
}
