#!/bin/sh
# Fails if the repo pulls in code that docs/licence-audit.md §4 forbids:
#   - Langfuse EE code (ee/ directories)
#   - vendored or imported AGPL code (Grafana, MinIO, Garage)
set -eu
cd "$(dirname "$0")/.."
fail=0

if git ls-files | grep -E '(^|/)ee/' >/dev/null; then
	echo "forbidden: files under an ee/ directory (Langfuse EE licence)"; fail=1
fi

# Go modules: no AGPL projects. @grafana/* npm packages need a per-package check (audit §3.4).
if grep -E 'github\.com/(grafana/grafana|minio/minio|deuxfleurs-org/garage)([/ ]|$)' go.mod >/dev/null 2>&1; then
	echo "forbidden: AGPL module in go.mod"; fail=1
fi
for pj in $(git ls-files '*package.json'); do
	if grep -E '"@grafana/' "$pj" >/dev/null; then
		echo "review needed: @grafana/* dependency in $pj (see licence-audit §3.4)"; fail=1
	fi
done

[ "$fail" -eq 0 ] && echo "guard: ok"
exit "$fail"
