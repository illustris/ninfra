# CLAUDE.md

Guidance for Claude Code when working in this repository.

## Project layout

- `src/ninfra.go` - the entire Go application (single file)
- `src/go.mod` - Go module (Go 1.21.6, no dependencies outside stdlib)
- `flake.nix` - package definition (`buildGoModule`), a sample `nixosConfigurations.target`, a fixture host `nixosConfigurations."web02.example.com"` (dotted name, no `targetHost`; used by the VM test to exercise attrpath quoting and host exclusion), custom `lib` helpers (`tern`, `right`, `indent`), and the NixOS VM test under `checks.x86_64-linux.ninfra-basic`

## Build and test

- Build: `nix build .#ninfra`
- Run: `nix run .#ninfra -- <subcommand> [options] [arguments]`
- Integration test: `nix flake check` (runs the `ninfra-basic` NixOS VM test, which boots a controller + target VM, deploys via `ninfra apply`, and asserts `ninfra compare` reports a match)
- There is no unit test suite. New behavior should be exercised through the VM test when feasible.

## Subcommands

- `exec <cmd...>` - SSH a command to each matching host
- `apply [action] [extra args...]` - run `nixos-rebuild` (default action: `switch`). Builds first, then applies. With a single matching host the build phase is implicitly skipped (the rebuild does it on the target). `--fast` skips the build phase explicitly. Arguments after the action are passed through to `nixos-rebuild` verbatim, in both the build and apply phases; they require an explicitly named action first (a leading unknown flag is rejected by the second flag-parsing pass).
- `compare` - SSH each host and `realpath /run/current-system /nix/var/nix/profiles/system`, then diff against the flake's evaluated closure path. Prints a table (HOST/ACTIVE/BOOT/PERSISTED/STATUS); `-v` adds a per-host detail report. Exits non-zero on mismatch, connection error, or remote error; SSH transport failures (exit 255) are reported as `Connection failed`, other remote failures as `Remote error`.

## CLI parsing

- Flags are parsed in two passes: once before the subcommand and once after it (`flag.CommandLine.Parse` on the remainder), so both `ninfra -l root apply` and `ninfra apply -l root` work. Parsing stops at the first non-flag argument, which leaves an `exec` command line and extra `apply` arguments untouched - ninfra flags for `exec` must come before the command to execute.
- If every host matching `--on` is excluded (e.g. no `targetHost`), `main` exits with an error rather than proceeding with an empty host set.

## Host selection and Nix expressions

- Hosts come from `.#nixosConfigurations`. The `--on` regex filters them.
- In the `nix eval` calls, the host is selected with `builtins.getAttr` inside the `--apply` expression (see `hostApplyExpr`/`nixQuoteString`) instead of interpolating it into the CLI attrpath, so any attribute name works - the Nix CLI attrpath syntax cannot represent quotes or backslashes at all. Do NOT pre-quote the attribute for `nixos-rebuild --flake` either - nixos-rebuild quotes the flake attribute itself, and pre-quoting breaks it.
- Three Nix expressions are configurable so the tool can adapt to flakes that don't follow the default convention:
  - `--list-hosts-expr` (default: `builtins.attrNames`)
  - `--deployment-expr` (default reads `config.deployment.targetHost` falling back to `config.passthru.deployment.targetHost`)
  - `--closure-path-expr` (default: `config.system.build.toplevel.outPath`)
- Closure paths are only evaluated for `apply` and `compare` (the `needsClosurePath` flag in `main`). `exec` skips that evaluation.
- Host listing, deployment-info evaluation, and closure-path evaluation all run in parallel bounded by `-P` (default: 1).

## Code conventions

- Tabs for indentation in both Go and Nix.
- Standard Go formatting; idiomatic stdlib-only style. Don't introduce dependencies without a strong reason - the empty `vendorHash = null` in `flake.nix` relies on this.
- Wrap errors with `fmt.Errorf("...: %w", err)` and include host context where relevant.
- For SSH and other subprocess work, capture stdout and stderr separately. Mixing them has caused error-classification bugs (treating SSH connection failures as state mismatches). Assign separate writers/buffers to `cmd.Stdout`/`cmd.Stderr` and use `cmd.Run()`; do not use `StdoutPipe`/`StderrPipe` with sequential or scanner-based reads - that pattern caused pipe-buffer deadlocks and a 64KB line-length hang.
- For streaming output with a host prefix, use the `lineWriter` type (no line-length limit; call `Flush` after `Run` for a partial trailing line).
- For direct SSH invocations, validate user-controlled strings before they reach `ssh` (see `isValidUsername`/`isValidHostname`). The `sshBaseArgs` helper centralizes this (and the shared `BatchMode`/`ConnectTimeout`/`StrictHostKeyChecking` options) for both `exec` and `compare`; `nixosRebuildArgs` validates the target for the apply/build actions.
- `NIX_SSHOPTS` is composed in `main` for nixos-rebuild's SSH connections: user-provided options come first (ssh honors the first occurrence, so they win), then ninfra's `BatchMode`, `ConnectTimeout`, and optional `StrictHostKeyChecking`.
- Always attach a context timeout via `newCmd` and call the returned `Cancel` (use `defer`) to avoid context leaks. `newCmd` also sets `cmd.WaitDelay` so descendants that inherit stdio (e.g. ssh spawned by nixos-rebuild) cannot hang `Wait` forever; treat `exec.ErrWaitDelay` as success-with-lost-output.
- Bound concurrency with a semaphore channel sized by `opts.Parallelism`. The existing pattern uses a `sync.WaitGroup` plus a buffered result channel closed by a separate goroutine - follow it for new parallel sections.

## Nix conventions

- Tabs for indentation.
- The `lib.indent` helper in `flake.nix` is used for embedded multi-line strings (e.g. `testScript`) - it strips a leading tab prefix inferred from the closing line. Mind the indentation when editing those strings.
