{
	description = "ninfra: an opinionated nixos infra management tool";
	inputs = {
		nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
	};
	outputs = { self, nixpkgs }: let
		forAllSystems = nixpkgs.lib.genAttrs [
			"x86_64-linux"
			"aarch64-linux"
			"x86_64-darwin"
			"aarch64-darwin"
		];
		testPkgs = nixpkgs.legacyPackages.x86_64-linux;
		ssh-keys = import "${nixpkgs}/nixos/tests/ssh-keys.nix" testPkgs;
		# Shared between the sample nixosConfigurations and the VM test, so
		# neither has to reach into the other's internals
		targetModules = rec {
			base = {
				networking.hostName = "target";
				passthru.deployment.targetHost = "target";
				services.openssh.enable = true;
				virtualisation.diskSize = 4096;
				users.users.root.openssh.authorizedKeys.keys = [
					ssh-keys.snakeOilPublicKey
				];
				# Use grub forceInstall to avoid ext2 embedding issues in VM
				boot.loader.grub.forceInstall = true;
			};
			modified = {
				imports = [ base ];
				# passthru merges shallowly (last-wins per top-level key), so
				# this must repeat the COMPLETE deployment set: adding only
				# healthChecks would clobber base's targetHost.
				passthru.deployment = {
					targetHost = "target";
					group = "web";
					maxConcurrentNodes = 1;
					failureDomain = "dc1";
					healthChecks = {
						exporterPort = { type = "tcp"; port = 9100; };
						exporterMetrics = { type = "http"; url = "http://target:9100/metrics"; };
						exporterActive = { type = "exec"; command = "systemctl is-active prometheus-node-exporter"; };
						exporterReady = { type = "output"; command = "systemctl is-active prometheus-node-exporter"; match = "^active"; };
					};
				};
				services.prometheus.exporters.node = {
					enable = true;
					openFirewall = true;
				};
			};
		};
		# VM wrapper shared by the deployable sample configs: they must stay
		# bootable under the test driver after a switch
		mkTarget = module: nixpkgs.lib.nixosSystem {
			system = "x86_64-linux";
			modules = [
				({ modulesPath, ... }: {
					imports = [
						(modulesPath + "/virtualisation/qemu-vm.nix")
						(modulesPath + "/testing/test-instrumentation.nix")
						module
					];
					# Disable the /etc/nix/registry.json pin of nixpkgs: it
					# would drag the 190MB nixpkgs source into every deployed
					# closure, and the slirp nix copy of it can outrun the
					# test's apply timeout, leaving the switch unapplied.
					nixpkgs.flake.setFlakeRegistry = false;
					nixpkgs.flake.setNixPath = false;
					# Preserve test network configuration after switch. The
					# addresses assume the test driver's alphabetical node
					# numbering on vlan 1 (controller=1, target=2).
					networking.interfaces.eth1.ipv4.addresses = [{
						address = "192.168.1.2";
						prefixLength = 24;
					}];
					networking.interfaces.eth1.ipv6.addresses = [{
						address = "2001:db8:1::2";
						prefixLength = 64;
					}];
				})
			];
		};
	in {
		lib = with self.lib; nixpkgs.lib // {
			tern = pred: x: y: if pred then x else y;
			right = f: g: tern (isFunction g)
				(right (x: f(g(x))))
				(f(g));
			indent = txt: let
				lines = splitString "\n" txt;
				# if the last line has n tabs, n+1 tabs need to be dropped from previous lines
				pivot = 1 + right stringLength last lines;
			in concatStringsSep "\n" (
				map (
					line: let
						rightStr = substring pivot (stringLength line) line;
						leftStr = substring 0 pivot line;
						# Check for non-tab characters in the truncated section
						# If you hit the following assert, check your indentation
					in assert (replaceStrings ["\t"] [""] leftStr == ""); rightStr
				) lines
			);
		};
		packages = forAllSystems (system: rec {
			ninfra = with nixpkgs.legacyPackages.${system}; callPackage (
				{ lib
				, buildGoModule
				}:
				buildGoModule rec {
					pname = "ninfra";
					version = "0.1.0";
					src = ./src;
					vendorHash = null;
					env.CGO_ENABLED = 0;
					# Runs `go test ./...` (planner/validation unit tests).
					doCheck = true;
				}
			) {};
			default = ninfra;
		});

		nixosConfigurations = {
			target = mkTarget targetModules.base;
			# Second generation of the same VM (adds the node exporter); the
			# VM test deploys it after target to exercise a real switch
			target-v2 = mkTarget targetModules.modified;

			# A host with a dotted attribute name and no deployment info at all
			# (neither deployment nor passthru.deployment is defined). ninfra
			# excludes it remotely; local mode can select it without deployment
			# metadata. The VM test exercises both paths and dotted host names.
			"web02.example.com" = nixpkgs.lib.nixosSystem {
				system = "x86_64-linux";
				modules = [{
					fileSystems."/" = { device = "/dev/vda1"; fsType = "ext4"; };
					boot.loader.grub.device = "nodev";
					system.stateVersion = "25.05";
				}];
			};
		};

		checks.x86_64-linux = {
			ninfra-basic = testPkgs.testers.nixosTest {
				name = "ninfra-basic";
				# Generous timeout: nix copy over SSH is slow, and every ninfra
				# invocation evaluates host configurations inside the VM
				globalTimeout = 3000;
				nodes = {
					controller = { pkgs, ... }: {
						nix.registry.nixpkgs.flake = nixpkgs;
						environment.systemPackages = [
							self.packages.x86_64-linux.ninfra
							(pkgs.writeShellScriptBin "setup-ssh-key" ''
								set -eu
								mkdir -p ~/.ssh
								install -m 0400 ${ssh-keys.snakeOilPrivateKey} ~/.ssh/id_ed25519
								ssh -o 'StrictHostKeyChecking accept-new' target hostname
							'')
						];
						environment.etc = {
							flake.source = self;
							nixpkgs.source = nixpkgs;
						};
						nix.settings.experimental-features = [ "nix-command" "flakes" ];
						virtualisation = {
							diskSize = 4096;
							memorySize = 4096;
							cores = 8;
						};
						system.extraDependencies = [
							self.nixosConfigurations.target.config.system.build.toplevel
							self.nixosConfigurations.target-v2.config.system.build.toplevel
						];
					};

					target = {
						imports = [ targetModules.base ];
						virtualisation = {
							# Seed evaluated generations and locked sources for offline local rebuilds.
							additionalPaths = [
								self
								nixpkgs
								self.packages.x86_64-linux.ninfra
								self.nixosConfigurations.target.config.system.build.toplevel
								self.nixosConfigurations.target-v2.config.system.build.toplevel
							];
							memorySize = 4096;
							cores = 4;
						};
					};
				};

				testScript = self.lib.indent ''
					import json
					import re
					import shlex

					def local_cmd(*args, path="/tmp/no-ssh:/run/current-system/sw/bin"):
					    words = [
					        "env", "-u", "USER",
					        "NIX_CONFIG=experimental-features = nix-command flakes\nflake-registry = ",
					        "PATH=" + path,
					        "${self.packages.x86_64-linux.ninfra}/bin/ninfra",
					        "--flake", "${self}", *args,
					    ]
					    return " ".join(shlex.quote(word) for word in words)

					controller.wait_for_unit("multi-user.target")
					target.wait_for_unit("sshd.service")

					# Setup SSH
					controller.succeed("setup-ssh-key")

					flake = controller.succeed("realpath /etc/flake").strip()

					# Verify flake is accessible (no --override-input needed with registry)
					controller.succeed(f"nix flake show {flake}")

					# Hosts without deployment info are excluded; a filter matching only
					# excluded hosts must fail instead of silently doing nothing. The dotted
					# attribute name exercises host name quoting, and --flake makes ninfra
					# independent of the cwd.
					(status, output) = controller.execute(f"ninfra --flake {flake} exec --on 'web02.*' true 2>&1")
					print(output)
					assert status != 0, f"Expected nonzero exit when all matched hosts are excluded, got {status}: {output}"
					assert "no target host configured" in output, f"Expected clean exclusion of web02.example.com, got: {output}"
					assert "no deployable hosts" in output, f"Expected 'no deployable hosts' error, got: {output}"

					# --on is anchored: a partial name must not substring-match
					(status, output) = controller.execute(f"ninfra --flake {flake} exec --on web02 true 2>&1")
					print(output)
					assert "no hosts matched the filter" in output, f"Expected anchored filter to match nothing, got: {output}"

					# Single output lines larger than 128KB must not stall the run
					# (the old 64KB scanner limit stopped pipe draining and hung);
					# exec arguments are shell-quoted argv-faithfully
					controller.succeed(f"cd {flake} && timeout 120 ninfra exec --on target -l root printf %0200000d 0")

					# Authentication failures must fail fast (BatchMode) instead of
					# hanging on an interactive password prompt
					(status, output) = controller.execute(f"cd {flake} && timeout 120 ninfra exec --on target -l nobody true 2>&1")
					print(output)
					assert status not in (0, 124), f"Expected fast auth failure, got {status}: {output}"

					# --timeout must terminate a stuck remote command instead of hanging
					(status, output) = controller.execute(f"cd {flake} && timeout 120 ninfra --timeout 5s exec --on target -l root sleep 300 2>&1")
					print(output)
					assert status not in (0, 124), f"Expected --timeout to kill the stuck command, got {status}: {output}"

					# Compare should show mismatch initially (N N N) and exit with code 1
					(status, output) = controller.execute(f"cd {flake} && ninfra compare --on target 2>&1")
					print(output)
					assert status == 1, f"Expected exit code 1 for mismatch, got: {status}"
					assert re.search(r'target\s+N\s+N\s+N', output), f"Expected initial mismatch, got: {output}"

					# Apply the first generation. nixos-rebuild --target-host sometimes
					# fails to exit after the switch completes, so a timeout (124) is
					# tolerated; the compare below is the real success criterion. The
					# cwd is read-only: all nixos-rebuild work happens in per-host temp
					# directories. A preexisting NIX_SSHOPTS must not break the apply
					# (this does not prove append-vs-clobber, only well-formedness).
					# 600s: the full base closure copy over slirp + DNS stalls can
					# exceed 300s, and a killed copy leaves the switch unapplied.
					(status, output) = controller.execute(f"cd {flake} && NIX_SSHOPTS='-o ConnectTimeout=30' timeout 600 ninfra apply --on target -l root 2>&1")
					print(f"Apply exit status: {status}")
					print(output)
					assert status in (0, 124), f"Apply failed with status {status}: {output}"

					# Wait for target to come back up after switch (network reconfiguration)
					target.wait_for_unit("sshd.service")
					target.wait_for_unit("network-addresses-eth1.service")

					# First generation active: compare should match (Y Y Y); -v after
					# the subcommand must produce the verbose report
					output = controller.succeed(f"cd {flake} && ninfra compare -v --on target 2>&1")
					print(output)
					assert re.search(r'target\s+Y\s+Y\s+Y', output), f"Expected match after apply, got: {output}"
					assert "==== Compare Report ====" in output, f"Expected verbose report from post-subcommand -v, got: {output}"

					# Health checks are defined only on target-v2 (node exporter); before the v2
					# apply they must fail (port closed, unit missing)
					(status, output) = controller.execute(f"cd {flake} && ninfra health --on target-v2 -l root 2>&1")
					print(output)
					assert status == 1, f"Expected failing health checks before v2 apply, got {status}: {output}"
					assert "exporterPort" in output, f"Expected named tcp check row, got: {output}"

					# --check selects a single named check (still failing here)
					(status, output) = controller.execute(f"cd {flake} && ninfra health --on target-v2 -l root --check exporterPort 2>&1")
					print(output)
					assert status == 1 and "exporterPort" in output and "exporterMetrics" not in output, f"Expected only exporterPort, got: {output}"

					# --ignore-check subtracting everything is not an error
					controller.succeed(f"cd {flake} && ninfra health --on target-v2 -l root --ignore-check 'exporter.*' 2>&1")

					# A host with no health checks defined is not an error
					controller.succeed(f"cd {flake} && ninfra health --on target -l root 2>&1")

					# Deploy the second generation (target-v2: same VM, adds the node
					# exporter) to exercise a real configuration switch
					(status, output) = controller.execute(f"cd {flake} && timeout 600 ninfra apply --on target-v2 -l root --health-timeout 3m 2>&1")
					print(f"Second apply exit status: {status}")
					print(output)
					assert status in (0, 124), f"Second apply failed with status {status}: {output}"
					assert "Waiting for health checks" in output, f"Expected health gate in rolling apply: {output}"

					target.wait_for_unit("sshd.service")
					target.wait_for_unit("network-addresses-eth1.service")
					target.wait_for_unit("prometheus-node-exporter.service")

					# The new generation matches target-v2 (machine-readable check)...
					out = controller.succeed(f"cd {flake} && ninfra compare --on target-v2 -json 2>/dev/null")
					print(out)
					results = json.loads(out)
					assert len(results) == 1 and results[0]["host"] == "target-v2", f"Unexpected JSON results: {out}"
					assert results[0]["activeMatch"] and results[0]["bootMatch"] and results[0]["persisted"], f"Expected full match for target-v2: {out}"

					# All four named health checks must pass after the v2 apply
					# (machine-readable)
					out = controller.succeed(f"cd {flake} && ninfra health --on target-v2 -l root -json 2>/dev/null")
					print(out)
					rows = json.loads(out)
					assert len(rows) == 4 and all(r["ok"] for r in rows), f"Expected all health checks to pass: {out}"
					assert sorted(r["check"].split(" ")[0] for r in rows) == ["exporterActive", "exporterMetrics", "exporterPort", "exporterReady"], f"Expected named checks in JSON: {out}"

					# ...and drifts from the original target config, which compare
					# must report as a mismatch (active/boot changed, persisted)
					(status, output) = controller.execute(f"cd {flake} && ninfra compare --on target 2>&1")
					print(output)
					assert status == 1, f"Expected exit code 1 for drift, got: {status}"
					assert re.search(r'target\s+N\s+N\s+Y', output), f"Expected drift after second apply, got: {output}"

					# Local mode uses the exact hostname, without USER or SSH.
					target.succeed("mkdir -p /tmp/no-ssh && printf '#!/bin/sh\\ntouch /tmp/local-ssh-used\\nexit 99\\n' > /tmp/no-ssh/ssh && chmod +x /tmp/no-ssh/ssh")
					assert target.succeed("hostname").strip() == "target"
					v1 = "${self.nixosConfigurations.target.config.system.build.toplevel}"
					v2 = "${self.nixosConfigurations.target-v2.config.system.build.toplevel}"
					(status, output) = target.execute(local_cmd("--local", "compare", "-json") + " 2>/dev/null")
					print(output)
					results = json.loads(output)
					assert status == 1 and len(results) == 1 and results[0]["host"] == "target", output
					row = results[0]
					assert (row["activeMatch"], row["bootMatch"], row["persisted"]) == (False, False, True), output
					assert "error" not in row and "connectionError" not in row, output
					for compare_args in [
					    ("compare", "--local", "--on", "target-v2", "-json"),
					    ("--on", "target-v2", "compare", "--local", "-json"),
					]:
					    output = target.succeed(local_cmd(*compare_args) + " 2>/dev/null")
					    results = json.loads(output)
					    assert len(results) == 1 and results[0]["host"] == "target-v2", output
					    row = results[0]
					    assert row["activeMatch"] and row["bootMatch"] and row["persisted"], output
					output = target.succeed(local_cmd("compare", "--local", "--on", "target-v2", "-v") + " 2>&1")
					print(output)
					assert re.search(r"target-v2\s+Y\s+Y\s+Y", output), output
					assert "==== Compare Report ====" in output, output
					assert "Compare command for target-v2: realpath [" in output, output
					assert "Compare command for target-v2: ssh" not in output, output

					# Selection must fail before any command or per-host metadata evaluation.
					(status, output) = target.execute(local_cmd("exec", "--local", "--on", ".*", "touch", "/tmp/local-selection-ran") + " 2>&1")
					print(output)
					assert status == 1 and "--local requires exactly one host; --on matched 3: target, target-v2, web02.example.com" in output, output
					target.succeed("test ! -e /tmp/local-selection-ran")
					(status, output) = target.execute(local_cmd("exec", "--local", "--on", "missing-local-host", "touch", "/tmp/local-selection-ran") + " 2>&1")
					assert status == 1 and "no hosts matched the filter" in output, output
					target.succeed("test ! -e /tmp/local-selection-ran")
					assert controller.succeed("hostname").strip() == "controller"
					(status, output) = controller.execute(local_cmd("exec", "--local", "true") + " 2>&1")
					assert status == 1 and "no hosts matched the filter" in output and "controller" in output, output
					for failure_args, diagnostic, evaluation_error in [
					    (("exec", "--local", "--deployment-expr", 'cfg: throw "local-deployment-error"', "touch", "/tmp/local-selection-ran"), "error evaluating deployment info for target", "local-deployment-error"),
					    (("compare", "--local", "--closure-path-expr", 'cfg: throw "local-closure-error"'), "error evaluating closure path for target", "local-closure-error"),
					    (("health", "--local", "--deployment-expr", 'cfg: { healthChecks = "malformed"; }'), "failed to unmarshal deployment info for target", "cannot unmarshal"),
					]:
					    (status, output) = target.execute(local_cmd(*failure_args) + " 2>&1")
					    print(output)
					    assert status == 1 and diagnostic in output, output
					    assert evaluation_error in output, output
					    assert "Error: no usable local configuration: selected host was excluded (see messages above)" in output, output
					target.succeed("test ! -e /tmp/local-selection-ran")

					# Kernel hostname changes select literal keys, including deployment-less FQDNs.
					try:
					    target.succeed("hostname target.example.com")
					    (status, output) = target.execute(local_cmd("exec", "--local", "true") + " 2>&1")
					    assert status == 1 and "no hosts matched the filter" in output, output
					    target.succeed("hostname web02.example.com")
					    output = target.succeed(local_cmd("exec", "--local", "printf", "%s", "local-only") + " 2>&1")
					    assert "web02.example.com: local-only" in output, output
					    output = target.succeed(local_cmd("health", "--local", "-json") + " 2>/dev/null")
					    assert json.loads(output) == [{"host": "web02.example.com", "ok": True, "detail": "no health checks defined"}], output
					    (status, output) = target.execute(local_cmd("compare", "--local", "--closure-path-expr", 'cfg: "/not-the-current-system"', "-json") + " 2>/dev/null")
					    results = json.loads(output)
					    assert status == 1 and len(results) == 1, output
					    row = results[0]
					    assert row["host"] == "web02.example.com" and row["currentSystem"] == v2 and row["systemProfile"] == v2, output
					    assert not row["activeMatch"] and not row["bootMatch"] and row["persisted"] and "error" not in row, output
					finally:
					    target.succeed("hostname target")

					# Local exec is argv-faithful; only explicit shells interpret shell syntax.
					output = target.succeed(local_cmd("exec", "--local", "printf", "<%s>\\n", "a b", "", "$HOME;*", "a'b") + " 2>&1")
					assert re.findall(r"target: (<[^\n]*>)", output) == ["<a b>", "<>", "<$HOME;*>", "<a'b>"], output
					(status, output) = target.execute(local_cmd("exec", "--local", "sh", "-c", "exit 7") + " 2>&1")
					assert status == 1 and "target" in output and "exit status 7" in output, output
					(status, output) = target.execute("timeout 30 " + local_cmd("exec", "--local", "--timeout", "1s", "sleep", "300") + " 2>&1")
					assert status not in (0, 124) and "target" in output, output

					# Health commands retain shell features, while local exit 255 is not SSH failure.
					output = target.succeed(local_cmd("health", "--local", "--on", "target-v2", "-json") + " 2>/dev/null")
					rows = json.loads(output)
					assert len(rows) == 4 and all(row["ok"] for row in rows), output
					deployment_expr = 'cfg: { targetHost = "192.0.2.1"; healthChecks = { loopback = { type = "tcp"; port = 9100; }; metrics = { type = "http"; url = "http://127.0.0.1:9100/metrics"; }; shellReady = { type = "output"; command = "printf ready | tr a-z A-Z"; match = "^READY$"; }; exit255 = { type = "exec"; command = "exit 255"; }; }; }'
					health_args = ("health", "--local", "--deployment-expr", deployment_expr)
					(status, output) = target.execute(local_cmd(*health_args, "-json") + " 2>/dev/null")
					print(output)
					rows = {row["check"].split(" ")[0]: row for row in json.loads(output)}
					assert status == 1 and set(rows) == {"loopback", "metrics", "shellReady", "exit255"}, output
					assert all(rows[name]["ok"] for name in ["loopback", "metrics", "shellReady"]), output
					assert not rows["exit255"]["ok"] and "exit status 255" in rows["exit255"]["error"], output
					(status, output) = target.execute(local_cmd(*health_args) + " 2>&1")
					assert status == 1 and "FAIL" in output and "Connection failed" not in output, output
					output = target.succeed(local_cmd(*health_args, "--ignore-check", "exit255", "-json") + " 2>/dev/null")
					rows = json.loads(output)
					assert len(rows) == 3 and all(row["ok"] for row in rows), output
					explicit_host_expr = deployment_expr.replace('type = "tcp"; port = 9100;', 'type = "tcp"; port = 9100; host = "192.0.2.1";')
					(status, output) = target.execute(local_cmd("health", "--local", "--deployment-expr", explicit_host_expr, "-c", "1", "--check", "loopback", "-json") + " 2>/dev/null")
					rows = json.loads(output)
					assert status == 1 and len(rows) == 1 and not rows[0]["ok"], output

					# A VM-only failing realpath proves local command error classification.
					target.succeed("mkdir -p /tmp/fail-realpath && printf '#!/bin/sh\\necho local-realpath-failed >&2\\nexit 255\\n' > /tmp/fail-realpath/realpath && chmod +x /tmp/fail-realpath/realpath")
					failure_path = "/tmp/fail-realpath:/tmp/no-ssh:/run/current-system/sw/bin"
					(status, output) = target.execute(local_cmd("compare", "--local", "-json", path=failure_path) + " 2>/dev/null")
					results = json.loads(output)
					assert status == 1 and len(results) == 1, output
					row = results[0]
					assert "local-realpath-failed" in row["error"] and "exit status 255" in row["error"] and not row.get("connectionError"), output
					(status, output) = target.execute(local_cmd("compare", "--local", path=failure_path) + " 2>&1")
					assert status == 1 and "Local error" in output and "local command errors" in output, output
					assert "Connection failed" not in output and "Remote error" not in output, output

					# Local test changes only the active system; SSH is stopped before activation.
					target.succeed("systemctl stop sshd.service")
					output = target.succeed("timeout 600 " + local_cmd("apply", "--local", "--deployment-expr", "cfg: { }", "test", "--show-trace") + " 2>&1")
					print(output)
					assert "Single target node, implicitly skipping separate build phase." in output, output
					assert target.succeed("realpath /run/current-system").strip() == v1
					assert target.succeed("realpath /nix/var/nix/profiles/system").strip() == v2
					target.fail("systemctl is-active prometheus-node-exporter.service")
					(status, output) = target.execute(local_cmd("compare", "--local", "-json") + " 2>/dev/null")
					results = json.loads(output)
					assert status == 1 and len(results) == 1 and results[0]["host"] == "target", output
					row = results[0]
					assert (row["activeMatch"], row["bootMatch"], row["persisted"]) == (True, False, False), output

					# Explicit local switch restores v2 and polls the real exporter health checks.
					target.succeed("systemctl stop sshd.service")
					output = target.succeed("timeout 600 " + local_cmd("--local", "apply", "--on", "target-v2", "--health-timeout", "3m", "switch", "--show-trace") + " 2>&1")
					print(output)
					assert "Waiting for health checks" in output, output
					target.wait_for_unit("prometheus-node-exporter.service")
					assert target.succeed("hostname").strip() == "target"
					assert target.succeed("realpath /run/current-system").strip() == v2
					assert target.succeed("realpath /nix/var/nix/profiles/system").strip() == v2
					output = target.succeed(local_cmd("compare", "--local", "--on", "target-v2", "-json") + " 2>/dev/null")
					results = json.loads(output)
					assert len(results) == 1 and results[0]["host"] == "target-v2", output
					row = results[0]
					assert row["activeMatch"] and row["bootMatch"] and row["persisted"], output
					output = target.succeed(local_cmd("health", "--local", "--on", "target-v2", "-json") + " 2>/dev/null")
					rows = json.loads(output)
					assert len(rows) == 4 and all(row["ok"] for row in rows), output
					(status, output) = target.execute(local_cmd("compare", "--local", "-json") + " 2>/dev/null")
					results = json.loads(output)
					assert status == 1 and len(results) == 1 and results[0]["host"] == "target", output
					row = results[0]
					assert (row["activeMatch"], row["bootMatch"], row["persisted"]) == (False, False, True), output
					target.succeed("test ! -e /tmp/local-ssh-used")
				'';
			};
		};
	};
}
