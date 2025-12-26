# ninfra

`ninfra` is a lightweight tool for managing NixOS configurations across multiple hosts. It enables the execution of commands and the application of NixOS rebuild actions in parallel on a filtered set of hosts.

## Features

- Execute arbitrary commands on multiple hosts via SSH.
- Apply NixOS rebuild actions (`switch`, `boot`, `test`, etc.) with parallelism.
- Compare deployed system paths against configured paths.
- Filter hosts using regular expressions.

## Usage

```bash
ninfra <subcommand> [options] [arguments]
```

### Subcommands

- **exec**: Execute a command on the specified hosts.
- **apply**: Perform a NixOS rebuild action (`nixos-rebuild`) on the specified hosts.
- **compare**: Compare the deployed system paths with the configured paths from the flake.

### Options

#### Common Options

- `--on <filter>`: Apply a regex filter to target specific hosts (default: `.*`).
- `-l <username>`: Specify the username for the target hosts (default: current user).
- `-P <parallelism>`: Set the number of parallel jobs (default: `1`).
- `--timeout <duration>`: Timeout for remote commands (e.g., `5m`, `1m`).
- `-h`, `--help`: Display usage information.

#### SSH Options

- `--trust-new-keys`: Automatically trust new server keys (default: `true`).

#### Apply Options

- `--fast`: Skip the initial build phase.
- `--use-substitutes`: Use substitutes when rebuilding (default: `true`).

#### Compare Options

- `-v`: Enable verbose output showing detailed comparison report.

#### Advanced Options

- `--list-hosts-expr <expr>`: Nix expression to list hosts (applied to `.#nixosConfigurations`).
- `--deployment-expr <expr>`: Nix expression to extract deployment info.
- `--closure-path-expr <expr>`: Nix expression to extract system path.

### Arguments

- **exec**: Provide the command to execute on the target hosts.
- **apply**: Specify the rebuild action (`switch`, `boot`, etc.) and optional additional arguments.
- **compare**: No additional arguments required.
