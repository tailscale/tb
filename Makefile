usage:
	@echo "Usage: make [target]"
	@echo "Targets:"
	@echo "  push-tb  - push controller http://tb/ to fly.io"
	@echo "  push-tbw - push worker to fly.io"

push-tb:
	@flyctl deploy -c tb/fly.toml --vm-size=performance-2x .

# nuke-tbw-base:
# 	@flyctl machine list -a tb-no-secrets --json  | jq -r '.[] | select(.name == "base") | .id' | xargs fly machine -a tb-no-secrets destroy -f 

push-tbw: # nuke-tbw-base
	@flyctl machine run -c tbw/fly.toml --name=base-$$(date +%s) --autostart=false --restart=no --env=EXIT_ON_START=1 --region=sea .
