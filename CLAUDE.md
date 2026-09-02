# CLAUDE.md

Guidance for Claude Code when working in this repository.

## Project layout

- `src/ninfra.go` - the entire Go application (single file)
- `src/ninfra_test.go` - unit tests for the pure parts only: `planApplyWaves`, `validateDeployment`, `selectedChecks`, `describeCheck` (everything that needs no I/O; the planner is deliberately kept pure so scheduling is testable without VMs)
- `src/go.mod` - Go module (Go 1.21, no dependencies outside stdlib)
- `flake.nix` - package definition (`buildGoModule` with `doCheck = true`, built for all four common systems via `forAllSystems`), sample configs `nixosConfigurations.target` and `target-v2` (two generations of the same VM, built by the shared `mkTarget` wrapper from `targetModules.base`/`.modified` in the outer `let` - the test and the sample configs share these modules instead of reaching into each other; `modified` repeats the COMPLETE `passthru.deployment` set because `passthru` merges shallowly and a partial set would clobber `targetHost`), a fixture host `nixosConfigurations."web02.example.com"` (dotted name, no deployment info; used by the VM test to exercise host name quoting and clean exclusion), custom `lib` helpers (`tern`, `right`, `indent`), and the NixOS VM test under `checks.x86_64-linux.ninfra-basic`

## Build and test

- Build: `nix build .#ninfra` (runs `go test ./...` in the check phase)
- Run: `nix run .#ninfra -- <subcommand> [options] [arguments]`
- Unit tests: `cd src && go test ./...` (or just build through nix)
- Integration test: `nix flake check` (runs the `ninfra-basic` NixOS VM test, which boots a controller + target VM, deploys two generations via `ninfra apply` (`target`, then `target-v2`), asserts `ninfra compare` reports match/drift correctly (including via `-json`), runs `ninfra health` against target-v2 failing before and passing after the v2 apply, checks `--check`/`--ignore-check` selection, and asserts the apply output contains the health-gate line). Takes 10-15 minutes. The apply timeouts are 600s and the gated v2 apply uses `--health-timeout 3m`: the target VM has no outside DNS and slirp copies are slow, so do not shrink these; targets drop the nixpkgs registry/NIX_PATH pin and are store-seeded via `virtualisation.additionalPaths` for the same reason.

## Subcommands

- `exec <cmd...>` - SSH a command to each matching host. Arguments are shell-quoted (argv-faithful); shell features need an explicit `sh -c '...'`. Failures end with a per-host summary including exit codes.
- `apply [action] [extra args...]` - run `nixos-rebuild` (default action: `switch`). Builds on all hosts first, then applies; a build failure aborts before any host is switched. With a single matching host the separate build phase is implicitly skipped (a standalone pre-build is redundant when the apply-phase rebuild builds the closure anyway - and note that without `--build-host` the build runs locally, not on the target). `--fast` skips the build phase explicitly. Arguments after the action are passed through to `nixos-rebuild` verbatim, in both the build and apply phases; they require an explicitly named action first (a leading unknown flag is rejected by the second flag-parsing pass).
- `health` - run each selected host's `deployment.healthChecks` and print a per-check table; `-json` emits flat rows on stdout. Exit 1 on any failed check or connection error. `--check`/`--ignore-check` (anchored name regexes, compiled in `main` alongside the gate) narrow the checks; a host with zero selected checks prints one `OK` row (`no health checks defined` / `no health checks matched`) and never fails. tcp/http run on the controller; exec/output run remotely via `sshBaseArgs` + the command as a single argv element (the remote shell interprets it). Needs no closure paths (`needsClosurePath` stays compare-only).
- `compare` - SSH each host and `realpath /run/current-system /nix/var/nix/profiles/system`, then diff against the flake's evaluated closure path. Prints a table (HOST/ACTIVE/BOOT/PERSISTED/STATUS); `-v` adds a per-host detail report; `-json` emits machine-readable results on stdout (and keeps stdout pure JSON). Exits non-zero on mismatch, connection error, or remote error; SSH transport failures (exit 255) are reported as `Connection failed`, other remote failures as `Remote error`.

## Rolling apply, failure domains, and the health gate

- `validateDeployment` runs for every subcommand right after `parseConfigurations`: per-check schema validation (errors name host + check) and rolling-knob consistency (k must agree within a group, j across all hosts; unset = 0 = inherit default, never a conflict). `effectiveMaxConcurrentNodes`/`effectiveMaxConcurrentDomains` mirror that rule.
- `planApplyWaves` is PURE (no I/O, no opts access): waves are chunks of j sorted domains; per domain, one batch of all ungrouped hosts first, then each sorted group chunked into k-sized sorted batches. Degenerate input (no group/domain anywhere) returns 1 wave / 1 domain / 1 batch, so legacy all-at-once applies flow through the same code path. Keep it pure and keep the tests in `ninfra_test.go` covering it.
- `applyRolling` drives the plan: one shared semaphore sized by `-P` bounds every remote op (applies AND gate waits); domains of a wave run concurrently, batches within a domain sequentially, hosts within a batch concurrently. An atomic abort flag stops scheduling on the first batch or gate failure; in-flight hosts finish and the error lists `applied: [...] skipped: [...]`. The gate (`waitHealthy`, 2s poll, `--health-timeout`) runs after each batch for `switch`/`test` unless `--skip-health-checks`, concurrently for all batch hosts that have selected checks.

## CLI parsing

- Flags are parsed in two passes: once before the subcommand and once after it (`flag.CommandLine.Parse` on the remainder), so both `ninfra -l root apply` and `ninfra apply -l root` work. Parsing stops at the first non-flag argument, which leaves an `exec` command line and extra `apply` arguments untouched - ninfra flags for `exec` must come before the command to execute.
- If every host matching `--on` is excluded (e.g. no `targetHost`), `main` exits with an error rather than proceeding with an empty host set.

## Host selection and Nix expressions

- Hosts come from `<flake>#nixosConfigurations`, where `<flake>` is `--flake` (default `.`; local directories are resolved to absolute paths in `main` so per-host working directories don't break relative refs). The `--on` regex filters them and is anchored (`^(?:...)$`): substring selection needs an explicit `.*`.
- In the `nix eval` calls, the host is selected with `builtins.getAttr` inside the `--apply` expression (see `hostApplyExpr`/`nixQuoteString`) instead of interpolating it into the CLI attrpath, so any attribute name works - the Nix CLI attrpath syntax cannot represent quotes or backslashes at all. Do NOT pre-quote the attribute for `nixos-rebuild --flake` either - nixos-rebuild quotes the flake attribute itself, and pre-quoting breaks it.
- Three Nix expressions are configurable so the tool can adapt to flakes that don't follow the default convention:
  - `--list-hosts-expr` (default: `builtins.attrNames`)
  - `--deployment-expr` (default reads the whole `config.deployment` set falling back to `config.passthru.deployment` - whole-set precedence, no per-field merge - wrapped in `builtins.tryEval`: `or` only catches missing attributes, and accessing a declared-but-never-defined option like `passthru` throws, so hosts with no deployment info at all are cleanly excluded as "no target host configured". JSON `null` unmarshals as a no-op into `DeploymentInfo`'s string/int/map fields, so the expr's `or null`/`or { }` per-field fallbacks need no Go-side handling)
  - `--closure-path-expr` (default: `cfg: cfg.config.system.build.toplevel.outPath`) - a function of the per-host `cfg`, like `--deployment-expr`, so the shown default is the verbatim value (unlike `--list-hosts-expr`'s `builtins.attrNames`, which is applied to the whole set directly rather than wrapped by `hostApplyExpr`)
- Closure paths are only evaluated for `compare` (the `needsClosurePath` flag in `main`); `apply`, `exec`, and `health` skip that evaluation - nothing in their pipelines consumes the path.
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
- `applyWithBuild` runs every nixos-rebuild in a per-host temporary working directory (`inWorkDir`): a shared cwd under `-P > 1` races on the `./result` out-link, loses GC roots between the build and apply phases, and litters the flake checkout. Keep any new nixos-rebuild invocations inside these work dirs. For build-style actions the dirs are kept (the result symlink is the deliverable). Consequence: relative paths in pass-through args or `NIX_SSHOPTS` must be absolute. The apply phase goes through `applyRolling` (not `executeConcurrently`); `executeConcurrently` remains for `exec` and the build phase.
- Health checks: `runHealthCheck` classifies ssh exit 255 as a connection error (`connErr`, mirroring `getCompareResults`) and treats `exec.ErrWaitDelay` as success; exec/output commands are a single argv element AFTER the `--` separator (remote shell interprets it - do not shell-quote it, features are a feature). `waitHealthy` polls, it does not single-shot: a gate wait re-runs all selected checks every 2s until `opts.HealthTimeout` passes.

## Nix conventions

- Tabs for indentation.
- The `lib.indent` helper in `flake.nix` is used for embedded multi-line strings (e.g. `testScript`) - it strips a leading tab prefix inferred from the closing line. Mind the indentation when editing those strings.
