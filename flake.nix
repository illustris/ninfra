{
	description = "A very basic flake";
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
					ldflags = [ "-X main.deployFilePath=${copyPathToStore ./src/deploy.nix}" ];
					CGO_ENABLED = 0;
				}
			) {};
			default = ninfra;
		};
	};
}
