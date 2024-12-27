x: let
	inherit (x.${builtins.head (builtins.attrNames x)}) pkgs lib;
in with lib; pipe x [
	(mapAttrs (_: v: attrByPath [
		# check for deployment.targetHost for colmena compat
		"config" "deployment" "targetHost"
	] (if v.options.passthru.definitions != [] then (attrByPath [
		# check for passthru.deployment.targetHost
		"config" "passthru" "deployment" "targetHost"
		# return null if not found
	] null v) else null) v))
	# remove nulls
	(filterAttrs (_: v: v != null))
]
