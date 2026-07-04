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
			# excludes it from all operations; the VM test uses it to exercise
			# host name quoting and clean host-exclusion handling.
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

					target = targetModules.base;
				};

				testScript = self.lib.indent ''
					import json
					import re

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
					(status, output) = controller.execute(f"cd {flake} && NIX_SSHOPTS='-o ConnectTimeout=30' timeout 300 ninfra apply --on target -l root 2>&1")
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

					# Deploy the second generation (target-v2: same VM, adds the node
					# exporter) to exercise a real configuration switch
					(status, output) = controller.execute(f"cd {flake} && timeout 300 ninfra apply --on target-v2 -l root 2>&1")
					print(f"Second apply exit status: {status}")
					print(output)
					assert status in (0, 124), f"Second apply failed with status {status}: {output}"

					target.wait_for_unit("sshd.service")
					target.wait_for_unit("network-addresses-eth1.service")
					target.wait_for_unit("prometheus-node-exporter.service")

					# The new generation matches target-v2 (machine-readable check)...
					out = controller.succeed(f"cd {flake} && ninfra compare --on target-v2 -json 2>/dev/null")
					print(out)
					results = json.loads(out)
					assert len(results) == 1 and results[0]["host"] == "target-v2", f"Unexpected JSON results: {out}"
					assert results[0]["activeMatch"] and results[0]["bootMatch"] and results[0]["persisted"], f"Expected full match for target-v2: {out}"

					# ...and drifts from the original target config, which compare
					# must report as a mismatch (active/boot changed, persisted)
					(status, output) = controller.execute(f"cd {flake} && ninfra compare --on target 2>&1")
					print(output)
					assert status == 1, f"Expected exit code 1 for drift, got: {status}"
					assert re.search(r'target\s+N\s+N\s+Y', output), f"Expected drift after second apply, got: {output}"
				'';
			};
		};
	};
}
