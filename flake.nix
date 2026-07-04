{
	description = "ninfra: an opinionated nixos infra management tool";
	inputs = {
		nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
	};
	outputs = { self, nixpkgs }: {
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
		packages.x86_64-linux = rec {
			ninfra = with nixpkgs.legacyPackages.x86_64-linux; callPackage (
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
		};

		nixosConfigurations.target = nixpkgs.lib.nixosSystem {
			system = "x86_64-linux";
			modules = [
				({ modulesPath, ... }: {
					imports = [
						(modulesPath + "/virtualisation/qemu-vm.nix")
						(modulesPath + "/testing/test-instrumentation.nix")
						self.checks.x86_64-linux.ninfra-basic.targetModules.base
					];
					# Preserve test network configuration after switch
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

		# A host with a dotted attribute name and no deployment.targetHost.
		# ninfra excludes it from all operations; the VM test uses it to
		# exercise Nix attrpath quoting and host-exclusion handling.
		nixosConfigurations."web02.example.com" = nixpkgs.lib.nixosSystem {
			system = "x86_64-linux";
			modules = [{
				fileSystems."/" = { device = "/dev/vda1"; fsType = "ext4"; };
				boot.loader.grub.device = "nodev";
				system.stateVersion = "25.05";
				# Defined but empty, so the default deployment expression
				# falls through to null instead of erroring on the
				# undefined passthru option
				passthru = { };
			}];
		};

		checks.x86_64-linux = {
			ninfra-basic = let
				targetModules = rec {
					base = { pkgs, ... }: {
						passthru.deployment.targetHost = "target";
						services.openssh.enable = true;
						virtualisation.diskSize = 4096;
						users.users.root.openssh.authorizedKeys.keys = [
							ssh-keys.snakeOilPublicKey
						];
						# Use grub forceInstall to avoid ext2 embedding issues in VM
						boot.loader.grub.forceInstall = true;
					};
					modified = { pkgs, ... }: {
						imports = [ base ];
						services.prometheus.exporters.node = {
							enable = true;
							openFirewall = true;
						};
					};
				};
				ssh-keys = import "${nixpkgs}/nixos/tests/ssh-keys.nix" nixpkgs.legacyPackages.x86_64-linux;
			in nixpkgs.legacyPackages.x86_64-linux.testers.nixosTest {
				name = "ninfra-basic";
				# Generous timeout: nix copy over SSH is slow, and every ninfra
				# invocation evaluates host configurations inside the VM
				globalTimeout = 2400;
				nodes = {
					controller = { config, pkgs, lib, ... }: {
						imports = [ ];
						nix.registry.nixpkgs.flake = nixpkgs;
						environment.systemPackages = [
							self.packages.x86_64-linux.ninfra
							(pkgs.writeScriptBin "kek" (lib.concatLines [
								"set -x" "cd ~" "mkdir .ssh"
								"cat ${ssh-keys.snakeOilPrivateKey} > ~/.ssh/id_ed25519"
								"chmod 0400 ~/.ssh/id_ed25519"
								"ssh -o 'StrictHostKeyChecking accept-new' -vvvvvv target hostname"
							]))
						];
						environment.etc = {
							flake.source = self;
							targetConfig.source = self.checks.x86_64-linux.ninfra-basic.nodes.target.config.system.build.toplevel;
							nixpkgs.source = nixpkgs;
						};
						nix.settings.experimental-features = [ "nix-command" "flakes" ];
						virtualisation = {
							diskSize = 4096;
							memorySize = 4096;
							cores = 8;
						};
						system.extraDependencies = [ self.nixosConfigurations.target.config.system.build.toplevel ];
					};

					target = targetModules.base;
				};

				passthru = { inherit targetModules; };

				testScript = self.lib.indent ''
					import re
					controller.wait_for_unit("multi-user.target")
					target.wait_for_unit("sshd.service")

					# Setup SSH
					controller.succeed("kek")

					# Verify flake is accessible (no --override-input needed with registry)
					controller.succeed("nix flake show $(realpath /etc/flake/)")

					# Copy flake to writable location (nixos-rebuild creates result symlink in cwd)
					controller.succeed("cp -r $(realpath /etc/flake) /tmp/flake")
					controller.succeed("chmod +w /tmp/flake")

					# Hosts without a deployment.targetHost are excluded; a filter matching
					# only excluded hosts must fail instead of silently doing nothing. The
					# dotted attribute name also exercises Nix attrpath quoting, and the
					# post-subcommand --on exercises the second flag-parsing pass.
					(status, output) = controller.execute("cd /tmp/flake && ninfra exec --on web02 true 2>&1")
					print(output)
					assert status != 0, f"Expected nonzero exit when all matched hosts are excluded, got {status}: {output}"
					assert "no target host configured" in output, f"Expected clean exclusion of web02.example.com, got: {output}"
					assert "no deployable hosts" in output, f"Expected 'no deployable hosts' error, got: {output}"

					# Single output lines larger than 128KB must not stall the run
					# (the old 64KB scanner limit stopped pipe draining and hung)
					controller.succeed("cd /tmp/flake && timeout 120 ninfra exec --on target -l root printf %0200000d 0")

					# Authentication failures must fail fast (BatchMode) instead of
					# hanging on an interactive password prompt
					(status, output) = controller.execute("cd /tmp/flake && timeout 120 ninfra exec --on target -l nobody true 2>&1")
					print(output)
					assert status not in (0, 124), f"Expected fast auth failure, got {status}: {output}"

					# --timeout must terminate a stuck remote command instead of hanging
					(status, output) = controller.execute("cd /tmp/flake && timeout 120 ninfra --timeout 5s exec --on target -l root sleep 300 2>&1")
					print(output)
					assert status not in (0, 124), f"Expected --timeout to kill the stuck command, got {status}: {output}"

					# Compare should show mismatch initially (N N N) and exit with code 1
					(status, output) = controller.execute("cd /tmp/flake && ninfra compare 2>&1")
					print(output)
					assert status == 1, f"Expected exit code 1 for mismatch, got: {status}"
					assert re.search(r'target\s+N\s+N\s+N', output), f"Expected initial mismatch, got: {output}"

					# Apply - nixos-rebuild --target-host has a known issue where it doesn't exit
					# after switch completes, so we use timeout and check output for success.
					# Flags after the subcommand are exercised here too, and a preexisting
					# NIX_SSHOPTS must not break the apply (this does not prove the
					# append-vs-clobber direction, only that the composition is well-formed).
					(status, output) = controller.execute("cd /tmp/flake && NIX_SSHOPTS='-o ConnectTimeout=30' timeout 300 ninfra apply -l root 2>&1")
					print(f"Apply exit status: {status}")
					print(output)
					# Accept exit 124 (timeout) if the switch actually succeeded
					if status == 124:
						assert "Installation finished" in output or "finished switching" in output, f"Apply timed out without completing: {output}"
					else:
						assert status == 0, f"Apply failed with status {status}"

					# Wait for target to come back up after switch (network reconfiguration)
					target.wait_for_unit("sshd.service")
					target.wait_for_unit("network-addresses-eth1.service")

					# Verify deployment succeeded - compare should now show match (Y Y Y).
					# -v after the subcommand must produce the verbose report.
					output = controller.succeed("cd /tmp/flake && ninfra compare -v 2>&1")
					print(output)
					assert re.search(r'target\s+Y\s+Y\s+Y', output), f"Expected match after apply, got: {output}"
					assert "==== Compare Report ====" in output, f"Expected verbose report from post-subcommand -v, got: {output}"
				'';
			};
		};
	};
}
