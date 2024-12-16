x: let
	inherit (x.${builtins.head (builtins.attrNames x)}) pkgs lib;
in with lib; (mapAttrs (n: v: v.config.deployment.targetHost) x)
