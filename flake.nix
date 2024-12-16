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

					# vendorHash = "sha256-k8UJef6AX26W1xLJJHRgrW+TGlbLnnkZxswJjqznmfM=";

					vendorHash = null;

					CGO_ENABLED = 0;

				}

			) {};

			default = ninfra;

		};

	};

}
