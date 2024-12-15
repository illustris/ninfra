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

func printHelp() {
	fmt.Println(`Usage: <subcommand> [options] [arguments]

Subcommands:
  exec               Execute a command on the specified hosts
  apply              Perform nixos-rebuild on the specified hosts

Options:
  --on <filter>       Set the filter to apply (default: .*)
  -l <username>       Specify the username for the target host (default: current user)
  -P <parallelism>    Specify the number of parallel jobs (default: 1)
  -h, --help          Display this help message

Arguments:
  For exec: Any command to execute on the target hosts
  For apply: An optional positional argument to specify the action (default: switch).
`)
}

func parseConfigurations() (Configurations, error) {
	data, err := ioutil.ReadFile("deploy.nix")
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

func executeConcurrently(configs map[string]string, args []string, username string, parallelism int, action ActionFunc) {
	semaphore := make(chan struct{}, parallelism)
	var wg sync.WaitGroup

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
				fmt.Println(err)
			}
		}(host, address)
	}

	wg.Wait() // Wait for all goroutines to finish
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
	return exec.Command("ssh", childArgs...)
}

func nixosRebuildAction(host, address string, args []string, username string) *exec.Cmd {
	if len(args) == 0 {
		args = []string{"switch"} // Default action
	}
	hostAttr := fmt.Sprintf(".#%s", host)
	sshTgt := fmt.Sprintf("%s@%s", username, address)
	return exec.Command("nixos-rebuild", args[0], "--flake", hostAttr, "--target-host", sshTgt)
}

func main() {
	filter := ".*"
	username := os.Getenv("USER")
	parallelism := 1
	subcommand := ""
	flag.StringVar(&filter, "on", filter, "Set the filter to apply (default: .*)")
	flag.StringVar(&username, "l", username, "Specify the username for the target host (default: current user)")
	flag.IntVar(&parallelism, "P", parallelism, "Specify the number of parallel jobs (default: 1)")
	help := flag.Bool("h", false, "Display this help message")
	flag.Parse()

	args := flag.Args()
	if len(args) > 0 {
		subcommand = args[0]
	}

	if *help || subcommand == "" {
		printHelp()
		os.Exit(0)
	}

	configs, err := parseConfigurations()
	if err != nil {
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
		executeConcurrently(filteredConfigs, args[1:], username, parallelism, nixosRebuildAction)

	default:
		fmt.Printf("Error: Unknown subcommand '%s'.\n", subcommand)
		printHelp()
		os.Exit(1)
	}
}
