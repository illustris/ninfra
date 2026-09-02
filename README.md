# ninfra

`ninfra` is a lightweight tool for managing NixOS configurations across multiple hosts. It enables the execution of commands and the application of NixOS rebuild actions in parallel on a filtered set of hosts.

## Features

- Execute arbitrary commands on multiple hosts via SSH.
- Apply NixOS rebuild actions (`switch`, `boot`, `test`, etc.) with parallelism.
- Rolling deploys: batch hosts by group and failure domain, gated by health checks.
- Health checks: TCP port, HTTP request, remote command exit status, and remote command output assertions, selectable by name.
- Compare deployed system paths against configured paths.
- Filter hosts using regular expressions.

## Usage

```bash
ninfra [options] <subcommand> [options] [arguments]
```

Options may be given before or after the subcommand. Option parsing stops at the first non-option argument, so for `exec`, ninfra options must come before the command to execute.

By default ninfra reads `nixosConfigurations` from the flake in the current directory; use `--flake <path or flake reference>` to point at another flake. Local paths are resolved to absolute paths, so ninfra works from any directory.

### Subcommands

- **exec**: Execute a command on the specified hosts. Arguments are shell-quoted so the remote argv matches what you typed locally; to use shell features (pipes, globs, variables), pass them through an explicit shell: `ninfra exec sh -c 'systemctl list-units | grep failed'`. If any host's command fails, ninfra prints a per-host summary (each host as `OK` or `failed (exit N)`) and exits non-zero.
- **apply**: Perform a NixOS rebuild action (`nixos-rebuild`) on the specified hosts. With multiple hosts, apply runs in two phases: it first builds the configuration for every matching host and only runs the action on them once all builds succeed, so a build failure aborts the whole apply before any host is changed. With a single matching host the separate build phase is skipped automatically (a standalone pre-build is redundant when the rebuild builds the closure anyway). Each host builds in its own temporary working directory, so parallel applies cannot race on a shared `result` symlink and your flake checkout stays clean; for build-style actions the per-host `result` symlinks are kept and their locations printed. Because of this, relative paths in pass-through arguments or `NIX_SSHOPTS` must be absolute.

  The apply phase is scheduled as waves: hosts declaring `deployment.failureDomain` are deployed at most `maxConcurrentDomains` domains at a time, and within a domain, hosts declaring `deployment.group` are switched in batches of the group's `maxConcurrentNodes` (ungrouped hosts go in a single batch first; hosts without group or domain keep the old all-at-once behavior). After each `switch`/`test` batch ninfra waits for the batch hosts' health checks to pass (`--health-timeout`, poll interval 2s) before moving on; `--skip-health-checks` disables the gate, and non-activating actions (e.g. `build`) skip it with a notice. Any failure stops further scheduling (in-flight hosts finish) and the error lists applied vs skipped hosts. There is no rollback.
- **health**: Run the deployment health checks defined on the selected hosts and print a per-check table (`HOST`, `CHECK`, `STATUS`, `DETAIL`; `OK` / `FAIL` / `Connection failed`). Exits non-zero if any check fails or a host cannot be reached. Hosts with no checks (or none matching the selection filters) print a single `OK` row. With `-json`, stdout is an array of flat rows `{"host": ..., "check": "name (type:target)", "ok": bool, "error": ..., "detail": ...}`. Checks are selected with `--check` / `--ignore-check` (anchored regexes on check names, like `--on`).
- **compare**: Compare the deployed system paths with the configured paths from the flake. Prints a table with columns HOST, ACTIVE, BOOT, PERSISTED, STATUS: ACTIVE is `Y` when the running system (`/run/current-system`) equals the flake's evaluated closure, BOOT is `Y` when the boot default (`/nix/var/nix/profiles/system`) equals it, and PERSISTED is `Y` when the running system equals the boot default (i.e. the active configuration will survive a reboot). Exits non-zero if any host mismatches, cannot be reached (`Connection failed`), or fails the remote check (`Remote error`).

### Output

Progress messages, the resolved "Selected hosts" listing, per-host command output (streamed live and prefixed with the host name, with stderr lines tagged `host (stderr):`), and failure summaries all go to **stderr**. `compare` and `health` write their tables to stdout (plus a trailing summary line and, for `compare`, the `-v` report); with `-json` stdout carries just the JSON payload. This keeps that output pipeable, but note that `exec` command output is on stderr, so redirecting stdout will not capture it.

### Options

#### Common Options

- `--on <filter>`: Regex to select hosts by name (default: `.*`). The pattern is anchored and must match the whole name: `--on web1` matches only the host `web1`, not `web10`; use `--on 'web1.*'` for prefix matching.
- `--flake <ref>`: Flake to read `nixosConfigurations` from (default: the current directory). Plain paths (`.`, `./infra`, `/srv/infra`) are resolved to absolute paths; anything else (registry names, `github:...` URLs) is passed to nix as-is. URL-style refs with relative paths (`path:./x`, `git+file:./x`) are not supported.
- `-l <username>`: Specify the username for the target hosts (default: current user).
- `-P <parallelism>`: Set the number of parallel jobs (default: `1`).
- `--timeout <duration>`: Timeout for remote commands (e.g., `5m`, `1m`; default: disabled).
- `-h`, `--help`: Display usage information.

#### SSH Options

- `--trust-new-keys`: Automatically trust new server keys (default: `true`).
- `-c <seconds>`: SSH connection timeout in seconds, applied to all SSH connections including the ones nixos-rebuild makes (default: `10`).

All SSH connections run with `BatchMode=yes`, so authentication failures fail fast instead of prompting for a password. For the `apply` build/rebuild phases, ninfra passes its SSH options to `nixos-rebuild` via `NIX_SSHOPTS`; anything you already have in `NIX_SSHOPTS` (e.g. `ProxyJump`, ports, identity files) is preserved and takes precedence.

#### Apply Options

- `--fast`: Skip the separate build phase. This phase is redundant for a single matching host and is skipped there automatically, so `--fast` only changes behavior for multi-host applies.
- `--use-substitutes`: Use substitutes when rebuilding (default: `true`).

#### Health Check Options

- `--health-timeout <duration>`: How long apply waits for a host's health checks to pass after a switch (default: `2m`).
- `--skip-health-checks`: Do not wait for health checks between rolling apply batches.
- `--check <regex>`: Select health checks by name (anchored: must match the whole name; default: `.*`). Applies to both the `health` subcommand and the apply gate.
- `--ignore-check <regex>`: Exclude health checks by name (anchored; applied after `--check`).

#### Compare and Health JSON Options

- `-v`: Enable verbose output showing detailed comparison report (compare only).
- `-json`: Output compare/health results as JSON on stdout (for CI/monitoring); exit-code semantics are unchanged and human messages stay off stdout. Compare output is an array of objects, each with `host` and `expected`, plus `currentSystem`/`systemProfile` (the deployed active/boot paths) on success. The booleans `activeMatch`, `bootMatch`, and `persisted` are always present but only meaningful when `error` is unset; on failure the object carries `error` and, for SSH/transport failures only, `connectionError: true`. Health output is an array of flat rows (see the `health` subcommand above).

#### Advanced Options

- `--list-hosts-expr <expr>`: Nix expression applied to the flake's `nixosConfigurations` to list host names (default: `builtins.attrNames`).
- `--deployment-expr <expr>`: Nix expression (a function of a host's configuration) to extract deployment info (default: reads the whole `config.deployment` set, falling back to `config.passthru.deployment`, so a host with neither is skipped; see "Deployment attributes" below).
- `--closure-path-expr <expr>`: Nix expression (a function of a host's configuration) to extract the system closure path (default: `cfg: cfg.config.system.build.toplevel.outPath`).

### Arguments

- **exec**: Provide the command to execute on the target hosts.
- **apply**: The rebuild action is optional and defaults to `switch`; name another action (`boot`, `test`, `dry-activate`, `build`, ...) as the first argument to override it. Any further arguments (e.g. `--install-bootloader`, `--show-trace`) are passed through to `nixos-rebuild` in both the build and apply phases. Passing extra arguments requires naming the action explicitly first: `ninfra apply switch --show-trace`.
- **compare**: No additional arguments required.
- **health**: No additional arguments required.

If every host matching the `--on` filter is excluded (for example because it has no `deployment.targetHost`), ninfra exits with an error instead of silently doing nothing.

### Deployment attributes

Deployment settings are read from the host's `config.deployment` set, falling back to `config.passthru.deployment` (typically set via `passthru.deployment = { ... };` in a NixOS module). The precedence is whole-set: if `config.deployment` exists it wins entirely — there is no per-field merge with `passthru.deployment`.

```nix
passthru.deployment = {
  targetHost = "web1";            # required; host is skipped without it
  group = "web";                  # optional; rolling-restart group
  maxConcurrentNodes = 2;         # hosts of this group switched at once (default 1; all hosts in a group that set it must agree)
  failureDomain = "dc1";          # optional; hosts without one form a single "" domain
  maxConcurrentDomains = 2;       # domains deployed at once (default 1; a global knob, so all hosts that set it must agree)
  healthChecks = {                # name-keyed; names are matched by --check/--ignore-check
    exporterPort = { type = "tcp"; port = 9100; };                              # optional host = "..." overrides the dial target (default: targetHost)
    metrics      = { type = "http"; url = "http://web1:9100/metrics"; };        # optional expectStatus (default: any 2xx passes)
    appActive    = { type = "exec"; command = "systemctl is-active myapp"; };    # run via ssh; pass = exit 0
    appReady     = { type = "output"; command = "myapp status"; match = "^ready"; }; # pass = exit 0 AND stdout matches the regex
  };
};
```

Check types:

- `tcp` — dial `host` (or `targetHost`) on `port` from the controller. Pass = connection succeeds.
- `http` — GET `url` from the controller. Pass = status is 2xx, or exactly `expectStatus` when set.
- `exec` — run `command` on the target over SSH (the remote shell interprets it, so pipes/redirections work). Pass = exit 0.
- `output` — like `exec`, but also requires stdout to match the `match` regex.

Validation runs before every operation: unknown types, out-of-range ports, empty URLs/commands, unparseable match regexes, and disagreeing `maxConcurrentNodes`/`maxConcurrentDomains` values abort with an error naming the host and check. Unset knobs (`0`) never conflict with set ones.

A custom `--deployment-expr` keeps working as long as it returns an attrset with these fields (missing fields become zero values); a healthChecks attrset whose values are not objects fails evaluation of that host.
