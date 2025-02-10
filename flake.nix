{
	description = "ninfra: an opinionated nixos infra management tool";
	inputs = {
		nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
	};
	outputs = { self, nixpkgs }: {
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
					ldflags = [ "-X main.DeployFilePath=${copyPathToStore ./src/deploy.nix}" ];
					env.CGO_ENABLED = 0;
				}
			) {};
			default = ninfra;
		};
	};
}
