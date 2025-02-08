x: let
	inherit (x.${builtins.head (builtins.attrNames x)}) pkgs lib;
in with lib; pipe x [
	(mapAttrs (_: v: {
		ip = v.config.deployment.targetHost or v.config.passthru.deployment.targetHost or null;
		path = v.config.system.build.toplevel.outPath;
	}))
	# remove nulls
	(filterAttrs (_: v: v != null))
]
