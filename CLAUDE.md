# CLAUDE.md

Guidance for Claude Code when working in this repository.

## Project layout

- `src/ninfra.go` - the entire Go application (single file)
- `src/go.mod` - Go module (Go 1.21, no dependencies outside stdlib)
- `flake.nix` - package definition (`buildGoModule`, built for all four common systems via `forAllSystems`), sample configs `nixosConfigurations.target` and `target-v2` (two generations of the same VM, built by the shared `mkTarget` wrapper from `targetModules.base`/`.modified` in the outer `let` - the test and the sample configs share these modules instead of reaching into each other), a fixture host `nixosConfigurations."web02.example.com"` (dotted name, no deployment info; used by the VM test to exercise host name quoting and clean exclusion), custom `lib` helpers (`tern`, `right`, `indent`), and the NixOS VM test under `checks.x86_64-linux.ninfra-basic`

## Build and test

- Build: `nix build .#ninfra`
- Run: `nix run .#ninfra -- <subcommand> [options] [arguments]`
- Integration test: `nix flake check` (runs the `ninfra-basic` NixOS VM test, which boots a controller + target VM, deploys two generations via `ninfra apply` (`target`, then `target-v2`), and asserts `ninfra compare` reports match/drift correctly, including via `-json`). Takes 10-15 minutes.
- There is no unit test suite. New behavior should be exercised through the VM test when feasible.

## Subcommands

- `exec <cmd...>` - SSH a command to each matching host. Arguments are shell-quoted (argv-faithful); shell features need an explicit `sh -c '...'`. Failures end with a per-host summary including exit codes.
- `apply [action] [extra args...]` - run `nixos-rebuild` (default action: `switch`). Builds on all hosts first, then applies; a build failure aborts before any host is switched. With a single matching host the separate build phase is implicitly skipped (a standalone pre-build is redundant when the apply-phase rebuild builds the closure anyway - and note that without `--build-host` the build runs locally, not on the target). `--fast` skips the build phase explicitly. Arguments after the action are passed through to `nixos-rebuild` verbatim, in both the build and apply phases; they require an explicitly named action first (a leading unknown flag is rejected by the second flag-parsing pass).
- `compare` - SSH each host and `realpath /run/current-system /nix/var/nix/profiles/system`, then diff against the flake's evaluated closure path. Prints a table (HOST/ACTIVE/BOOT/PERSISTED/STATUS); `-v` adds a per-host detail report; `-json` emits machine-readable results on stdout (and keeps stdout pure JSON). Exits non-zero on mismatch, connection error, or remote error; SSH transport failures (exit 255) are reported as `Connection failed`, other remote failures as `Remote error`.

## CLI parsing

- Flags are parsed in two passes: once before the subcommand and once after it (`flag.CommandLine.Parse` on the remainder), so both `ninfra -l root apply` and `ninfra apply -l root` work. Parsing stops at the first non-flag argument, which leaves an `exec` command line and extra `apply` arguments untouched - ninfra flags for `exec` must come before the command to execute.
- If every host matching `--on` is excluded (e.g. no `targetHost`), `main` exits with an error rather than proceeding with an empty host set.

## Host selection and Nix expressions

- Hosts come from `<flake>#nixosConfigurations`, where `<flake>` is `--flake` (default `.`; local directories are resolved to absolute paths in `main` so per-host working directories don't break relative refs). The `--on` regex filters them and is anchored (`^(?:...)$`): substring selection needs an explicit `.*`.
- In the `nix eval` calls, the host is selected with `builtins.getAttr` inside the `--apply` expression (see `hostApplyExpr`/`nixQuoteString`) instead of interpolating it into the CLI attrpath, so any attribute name works - the Nix CLI attrpath syntax cannot represent quotes or backslashes at all. Do NOT pre-quote the attribute for `nixos-rebuild --flake` either - nixos-rebuild quotes the flake attribute itself, and pre-quoting breaks it.
- Three Nix expressions are configurable so the tool can adapt to flakes that don't follow the default convention:
  - `--list-hosts-expr` (default: `builtins.attrNames`)
  - `--deployment-expr` (default reads `config.deployment.targetHost` falling back to `config.passthru.deployment.targetHost`, wrapped in `builtins.tryEval`: `or` only catches missing attributes, and accessing a declared-but-never-defined option like `passthru` throws, so hosts with no deployment info at all are cleanly excluded as "no target host configured")
  - `--closure-path-expr` (default: `cfg: cfg.config.system.build.toplevel.outPath`) - a function of the per-host `cfg`, like `--deployment-expr`, so the shown default is the verbatim value (unlike `--list-hosts-expr`'s `builtins.attrNames`, which is applied to the whole set directly rather than wrapped by `hostApplyExpr`)
- Closure paths are only evaluated for `compare` (the `needsClosurePath` flag in `main`); `apply` and `exec` skip that evaluation - nothing in the apply pipeline consumes the path.
- Deployment-info evaluation and closure-path evaluation fan out per host, bounded by `-P` (default: 1); host listing is a single serial `nix eval` over the whole `nixosConfigurations` set (`getHostsList`), not per-host and not affected by `-P`. Execution and output paths iterate hosts through `sortedHosts` (or sort the result slice) for deterministic order.

## Code conventions

- Tabs for indentation in both Go and Nix.
- Standard Go formatting; idiomatic stdlib-only style. Don't introduce dependencies without a strong reason - the empty `vendorHash = null` in `flake.nix` relies on this.
- Wrap errors with `fmt.Errorf("...: %w", err)` and include host context where relevant.
- For SSH and other subprocess work, capture stdout and stderr separately. Mixing them has caused error-classification bugs (treating SSH connection failures as state mismatches). Assign separate writers/buffers to `cmd.Stdout`/`cmd.Stderr` and use `cmd.Run()`; do not use `StdoutPipe`/`StderrPipe` with sequential or scanner-based reads - that pattern caused pipe-buffer deadlocks and a 64KB line-length hang.
- For streaming output with a host prefix, use the `lineWriter` type (no `bufio.Scanner`-style hard cap that errors or drops on long lines; complete lines up to `lineWriterFlushLimit` (1 MiB) are logged whole, and any longer newline-free run is flushed in ~1 MiB chunks to bound memory, so a line past that limit is split across records; call `Flush` after `Run` for a partial trailing line).
- For direct SSH invocations, validate user-controlled strings before they reach `ssh` (see `isValidUsername`/`isValidHostname`). The `sshBaseArgs` helper centralizes this (and the shared `BatchMode`/`ConnectTimeout`/`StrictHostKeyChecking` options) for both `exec` and `compare`; `nixosRebuildArgs` validates the target for the apply/build actions.
- `NIX_SSHOPTS` is composed in `main` for nixos-rebuild's SSH connections: user-provided options come first (ssh honors the first occurrence, so they win), then ninfra's `BatchMode`, `ConnectTimeout`, and optional `StrictHostKeyChecking`.
- Always attach a context timeout via `newCmd` and call the returned `Cancel` (use `defer`) to avoid context leaks. `newCmd` also sets `cmd.WaitDelay` so descendants that inherit stdio (e.g. ssh spawned by nixos-rebuild) cannot hang `Wait` forever; treat `exec.ErrWaitDelay` as success-with-lost-output.
- Bound concurrency with a semaphore channel sized by `opts.Parallelism`, a `sync.WaitGroup`, and a result channel buffered to the host count. Two close shapes exist: the execution paths (`executeConcurrently`, `getCompareResults`) close the channel inline after `wg.Wait()` and then drain it (safe because the buffer holds every result, so no send blocks); `getHostsConfigs` closes from a separate goroutine (`go func(){ wg.Wait(); close(ch) }()`) while ranging the channel live. Prefer the inline close when the buffer holds all results; use the goroutine close only when consuming concurrently with production.
- `applyWithBuild` runs every nixos-rebuild in a per-host temporary working directory (`inWorkDir`): a shared cwd under `-P > 1` races on the `./result` out-link, loses GC roots between the build and apply phases, and litters the flake checkout. Keep any new nixos-rebuild invocations inside these work dirs. For build-style actions the dirs are kept (the result symlink is the deliverable). Consequence: relative paths in pass-through args or `NIX_SSHOPTS` must be absolute.

## Nix conventions

- Tabs for indentation.
- The `lib.indent` helper in `flake.nix` is used for embedded multi-line strings (e.g. `testScript`) - it strips a leading tab prefix inferred from the closing line. Mind the indentation when editing those strings.
