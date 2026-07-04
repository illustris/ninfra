# ninfra

`ninfra` is a lightweight tool for managing NixOS configurations across multiple hosts. It enables the execution of commands and the application of NixOS rebuild actions in parallel on a filtered set of hosts.

## Features

- Execute arbitrary commands on multiple hosts via SSH.
- Apply NixOS rebuild actions (`switch`, `boot`, `test`, etc.) with parallelism.
- Compare deployed system paths against configured paths.
- Filter hosts using regular expressions.

## Usage

```bash
ninfra [options] <subcommand> [options] [arguments]
```

Options may be given before or after the subcommand. Option parsing stops at the first non-option argument, so for `exec`, ninfra options must come before the command to execute.

ninfra must be run from the root of the flake containing your `nixosConfigurations`; all evaluation uses the flake in the current directory (`.#`).

### Subcommands

- **exec**: Execute a command on the specified hosts.
- **apply**: Perform a NixOS rebuild action (`nixos-rebuild`) on the specified hosts.
- **compare**: Compare the deployed system paths with the configured paths from the flake. Exits non-zero if any host mismatches, cannot be reached (`Connection failed`), or fails the remote check (`Remote error`).

### Options

#### Common Options

- `--on <filter>`: Apply a regex filter to target specific hosts (default: `.*`).
- `-l <username>`: Specify the username for the target hosts (default: current user).
- `-P <parallelism>`: Set the number of parallel jobs (default: `1`).
- `--timeout <duration>`: Timeout for remote commands (e.g., `5m`, `1m`).
- `-h`, `--help`: Display usage information.

#### SSH Options

- `--trust-new-keys`: Automatically trust new server keys (default: `true`).
- `-c <seconds>`: SSH connection timeout in seconds, applied to all SSH connections including the ones nixos-rebuild makes (default: `10`).

All SSH connections run with `BatchMode=yes`, so authentication failures fail fast instead of prompting for a password. For the `apply` build/rebuild phases, ninfra passes its SSH options to `nixos-rebuild` via `NIX_SSHOPTS`; anything you already have in `NIX_SSHOPTS` (e.g. `ProxyJump`, ports, identity files) is preserved and takes precedence.

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
- **apply**: Specify the rebuild action (`switch`, `boot`, etc.); any further arguments (e.g. `--install-bootloader`, `--show-trace`) are passed through to `nixos-rebuild` in both the build and apply phases. Passing extra arguments requires naming the action explicitly first: `ninfra apply switch --show-trace`.
- **compare**: No additional arguments required.

If every host matching the `--on` filter is excluded (for example because it has no `deployment.targetHost`), ninfra exits with an error instead of silently doing nothing.
