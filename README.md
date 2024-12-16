# ninfra

`ninfra` is a lightweight tool for managing NixOS configurations across multiple hosts. It enables the execution of commands and the application of NixOS rebuild actions in parallel on a filtered set of hosts.

## Features

- Execute arbitrary commands on multiple hosts via SSH.
- Apply NixOS rebuild actions (`switch`, `boot`, `test`, etc.) with parallelism.
- Filter hosts using regular expressions.
- Dynamic configuration file lookup with build-time path specification.

## Usage

```bash
ninfra <subcommand> [options] [arguments]
```

### Subcommands

- **exec**: Execute a command on the specified hosts.
- **apply**: Perform a NixOS rebuild action (`nixos-rebuild`) on the specified hosts.

### Options

- `--on <filter>`: Apply a regex filter to target specific hosts (default: `.*`).
- `-l <username>`: Specify the username for the target hosts (default: current user).
- `-P <parallelism>`: Set the number of parallel jobs (default: `1`).
- `-d <deploy.nix>`: Use a custom nix expression file to generate the target and config attrset (default: builtin)
- `-h`, `--help`: Display usage information.

### Arguments

- **exec**: Provide the command to execute on the target hosts.
- **apply**: Specify the rebuild action (`switch`, `boot`, etc.) and optional additional arguments.
