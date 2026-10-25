#!/usr/bin/env bash
# install.sh: turn a fresh DGX Spark / GX10 into a complete single-box
# appliance with one command:
#
#   curl -fsSL https://raw.githubusercontent.com/anindyar/openagentfabric/main/install.sh \
#     | bash -s -- --email you@company.com
#
# OAF only (the AI platform: gateway, models, routing, observability). With
# --garrison it also installs the Garrison security console on top, enrols
# this box's own Garrison agent and makes it a log collector:
#
#   export GARRISON_GIT_TOKEN=<read access to the Garrison repository>
#   curl -fsSL https://raw.githubusercontent.com/anindyar/openagentfabric/main/install.sh \
#     | bash -s -- --email you@company.com --garrison
#
# Run as a user with sudo on DGX OS (docker present). Every step checks before
# it acts, so re-running after a failure continues where it stopped. More
# boxes join later from the console (docs/user-guide.md, "Adding boxes").
set -euo pipefail

OAF_REPO=${OAF_REPO:-https://github.com/anindyar/openagentfabric.git}
OAF_REF=${OAF_REF:-main}
GARRISON_REPO=${GARRISON_REPO:-https://github.com/anindyar/vclocal.git}
GARRISON_REF=${GARRISON_REF:-garrison}
SRC=/opt/oaf/src
GARRISON_DIR=/opt/garrison

email=""
address=""
garrison=false
self_enrol=true
bootstrap_args=()

usage() {
	cat <<EOF
Usage: install.sh --email you@company.com [options]

  --email ADDR        who accepts the licences (recorded in cluster.yaml); required
  --garrison          also install the Garrison security console (needs
                      GARRISON_GIT_TOKEN in the environment: read access to its repository)
  --no-self-enrol     with --garrison: do not enrol this box's own Garrison agent
  --address IP        management address (default: the source address of the default route)
  --name NAME         node name (default: the hostname)
  --no-model          install the platform only, no chat model
  --ref REF           OAF version (git ref) to install (default: $OAF_REF)

Anything else is passed to scripts/spark-bootstrap.sh (see its --help).
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--email) email=$2; bootstrap_args+=(--email "$2"); shift 2 ;;
	--garrison) garrison=true; shift ;;
	--no-self-enrol) self_enrol=false; shift ;;
	--ref) OAF_REF=$2; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	--address) address=$2; bootstrap_args+=(--address "$2"); shift 2 ;;
	--name | --util | --max-model-len | --weights-from | --oaf-bin | --health-timeout)
		bootstrap_args+=("$1" "$2"); shift 2 ;;
	*) bootstrap_args+=("$1"); shift ;;
	esac
done

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ -n "$email" ] || { usage >&2; die "--email is required"; }
# The same default as the bootstrap: the source address of the default route.
[ -n "$address" ] || address=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')
[ -n "$address" ] || die "no default route to take the management address from; pass --address"

# ------------------------------------------------------------------ preflight
step "Preflight"
[ "$(uname -s)" = Linux ] || die "Linux only"
case "$(uname -m)" in aarch64 | arm64 | x86_64) ;; *) die "unsupported architecture $(uname -m)" ;; esac
[ "$(id -u)" -ne 0 ] || die "run as a user with sudo, not as root (the console's files belong to that user)"
sudo -v || die "this installer needs sudo"
for c in git curl docker; do command -v "$c" >/dev/null || die "$c is missing (DGX OS ships docker; install git and curl with apt)"; done
docker compose version >/dev/null 2>&1 || sudo docker compose version >/dev/null 2>&1 || die "the docker compose plugin is missing"
free_gib=$(df -BG --output=avail /var/lib 2>/dev/null | tail -1 | tr -dc 0-9)
[ "${free_gib:-0}" -ge 150 ] || die "/var/lib has ${free_gib:-0} GiB free; models and images need at least 150 GiB"
# Never take over a box that already runs something else.
others=$(sudo docker ps --format '{{.Names}}' | grep -vE '^(oaf-|vclocal-)' || true)
if [ -n "$others" ]; then
	info "containers not managed here are running: $(echo "$others" | tr '\n' ' ')"
	info "they are left alone; make sure they leave room (memory, ports 3000-3100, 4000, 8000+, 8080, 8443, 9090)"
fi
if $garrison; then
	[ -n "${GARRISON_GIT_TOKEN:-}" ] || git ls-remote "$GARRISON_REPO" >/dev/null 2>&1 ||
		die "--garrison needs read access to $GARRISON_REPO: export GARRISON_GIT_TOKEN=<token> (it is passed to git through GIT_ASKPASS, never on a command line)"
fi
info "ok: $(uname -m), ${free_gib} GiB free, $(docker --version | cut -d, -f1)"

# --------------------------------------------------------------- source tree
fetch() { # repo ref dir
	local repo=$1 ref=$2 dir=$3
	sudo install -d -o "$(id -u)" -g "$(id -g)" "$(dirname "$dir")"
	if [ -d "$dir/.git" ]; then
		git -C "$dir" fetch -q origin "$ref"
		git -C "$dir" checkout -q -B "$ref" FETCH_HEAD
	else
		git clone -q --branch "$ref" "$repo" "$dir"
	fi
	info "$(basename "$dir"): $(git -C "$dir" log --oneline -1)"
}

step "OAF ($OAF_REF)"
fetch "$OAF_REPO" "$OAF_REF" "$SRC"

# ---------------------------------------------------------------------- OAF
step "Platform: OAF, models, gateway, observability"
# Under `curl | bash` this script itself is standard input: the licence
# prompt must read the terminal instead. Without one (automation), the
# licences have to be accepted with --accept-licences.
if [ -t 0 ]; then
	"$SRC/scripts/spark-bootstrap.sh" "${bootstrap_args[@]}"
elif [ -r /dev/tty ] && (exec </dev/tty) 2>/dev/null; then
	"$SRC/scripts/spark-bootstrap.sh" "${bootstrap_args[@]}" </dev/tty
else
	printf '%s\n' "${bootstrap_args[@]}" | grep -qx -- --accept-licences ||
		die "no terminal to accept the licences on: add --accept-licences (you accept the NVIDIA and model terms the bootstrap lists)"
	"$SRC/scripts/spark-bootstrap.sh" "${bootstrap_args[@]}" </dev/null
fi

$garrison || {
	step "Done"
	info "OAF is running on this box. Status: sudo oaf status"
	info "Add boxes later from the console, or see docs/user-guide.md, \"Adding boxes\"."
	exit 0
}

# ----------------------------------------------------------------- Garrison
step "Garrison ($GARRISON_REF)"
# The token reaches git through an askpass helper: never in argv, the process
# list, shell history or .git/config.
askpass=""
if [ -n "${GARRISON_GIT_TOKEN:-}" ]; then
	askpass=$(mktemp)
	chmod 700 "$askpass"
	printf '#!/bin/sh\ncase "$1" in Username*) echo x-access-token ;; *) echo "$GARRISON_GIT_TOKEN" ;; esac\n' >"$askpass"
	export GIT_ASKPASS=$askpass GIT_TERMINAL_PROMPT=0
	trap 'rm -f "$askpass"' EXIT
fi
fetch "$GARRISON_REPO" "$GARRISON_REF" "$GARRISON_DIR"
[ -n "$askpass" ] && { rm -f "$askpass"; unset GIT_ASKPASS; }

sudo "$GARRISON_DIR/deployments/dgx-spark/install.sh" --host-ip "$address" --oaf --admin-email "$email" --yes

# --------------------------------------------------- this box into Garrison
if $self_enrol; then
	step "This box into Garrison (agent and log collector)"
	"$GARRISON_DIR/deployments/dgx-spark/self-enrol.sh" "https://$address:8080"
fi

step "Done"
info "Garrison:  https://$address/   (admin: $email; the password is in $GARRISON_DIR/.env, ADMIN_PASSWORD)"
info "Platform:  sudo oaf status"
info "Add boxes from Garrison: Cluster -> Add boxes (docs/user-guide.md, \"Adding boxes\")."
