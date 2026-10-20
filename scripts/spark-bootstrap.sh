#!/usr/bin/env bash
# spark-bootstrap.sh: turn a fresh DGX Spark / GX10 into a one-box OAF node.
#
# Run it on the new box, from a checkout of this repo, as a user with sudo:
#
#   git clone https://github.com/anindyar/openagentfabric && cd openagentfabric
#   scripts/spark-bootstrap.sh --email you@example.com
#
# It installs the oaf binary and node agent, writes /etc/oaf/cluster.yaml
# (topology single: the full OAF stack plus Qwen3.8 Flash-Next at 0.75 of
# unified memory), fetches the weights, deploys, waits until everything is
# healthy and sends a test request through the gateway. Every step checks
# before it acts, so the script is safe to re-run after a failure.
#
# It never touches containers that are not OAF's (compose projects oaf-*).
# This is the interim path until `curl … | bash` (M5); see docs/user-guide.md §4.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
MODEL_ID=qwen3.8-flash-next-fp4
DEPLOYMENT=qwen-flash
IMAGE_COMPONENT=vllm-spark-b12x
MODEL_LICENCE_URL=https://www.nvidia.com/en-us/agreements/enterprise-software/nvidia-open-model-agreement/
CUDA_EULA_URL=https://docs.nvidia.com/cuda/eula/index.html
# Builds oaf when the box has no Go toolchain (it usually has none).
GO_IMAGE=docker.io/library/golang:1.27-bookworm@sha256:5c3e9b43cfd3bb95c7937462e29d87ada57b481a96fa23ad0eb61778d7a00110

ETC=/etc/oaf
PKI=$ETC/pki
MODELS=/var/lib/oaf/models

name=$(hostname -s | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-')
address=""
email=""
util=0.75
max_len=262144
accept=false
weights_from=""
with_model=true
oaf_bin=""
rewrite_config=false
health_timeout=60 # minutes; a cold start autotunes b12x kernels for ~10 min

usage() {
	cat <<EOF
Usage: $0 --email you@example.com [options]

  --email ADDR          who accepts the licences (recorded in cluster.yaml); required
  --name NAME           node name, a DNS label (default: $name)
  --address IP          management address (default: the source address of the default route)
  --util FRACTION       vLLM --gpu-memory-utilization (default: $util)
  --max-model-len N     context length (default: $max_len)
  --weights-from SRC    rsync source for the weights instead of Hugging Face,
                        e.g. user@other-spark:/var/lib/oaf/models/$MODEL_ID/
  --no-model            install the stack only, no model
  --oaf-bin PATH        use this oaf binary instead of building one
  --rewrite-config      overwrite an existing $ETC/cluster.yaml
  --accept-licences     accept the licences without the prompt (CI, re-runs)
  --health-timeout MIN  minutes to wait for the model (default: $health_timeout)

HF_TOKEN in the environment is passed to the download if set (the model is not gated).
EOF
}

while [ $# -gt 0 ]; do
	case $1 in
	--email) email=$2; shift 2 ;;
	--name) name=$2; shift 2 ;;
	--address) address=$2; shift 2 ;;
	--util) util=$2; shift 2 ;;
	--max-model-len) max_len=$2; shift 2 ;;
	--weights-from) weights_from=$2; shift 2 ;;
	--no-model) with_model=false; shift ;;
	--oaf-bin) oaf_bin=$2; shift 2 ;;
	--rewrite-config) rewrite_config=true; shift ;;
	--accept-licences | --accept-licenses) accept=true; shift ;;
	--health-timeout) health_timeout=$2; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
	esac
done

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ -n "$email" ] || { usage >&2; die "--email is required"; }
[[ $name =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || die "node name '$name' is not a DNS label; pass --name"
[ -n "$address" ] || address=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')
[ -n "$address" ] || die "cannot find the management address; pass --address"

# ---------------------------------------------------------------------------
step "Host checks"
[ "$(uname -m)" = aarch64 ] || die "this is $(uname -m); OAF targets GB10 (aarch64)"
command -v docker >/dev/null || die "docker is missing (DGX OS ships it)"
docker compose version >/dev/null 2>&1 || die "the docker compose plugin is missing"
sudo -v || die "this script needs sudo"
DOCKER="docker"
docker info >/dev/null 2>&1 || DOCKER="sudo docker"
image=$(awk -v c="$IMAGE_COMPONENT" '$0 ~ "- name: "c"$" {f=1} f && /image:/ {print $2; exit}' "$REPO_ROOT/bundle/versions.yaml")
[ -n "$image" ] || die "bundle/versions.yaml pins no $IMAGE_COMPONENT"
[ -e /var/run/cdi/nvidia.yaml ] || [ -e /etc/cdi/nvidia.yaml ] ||
	die "no NVIDIA CDI spec (/var/run/cdi/nvidia.yaml); containers cannot get the GPU"
mem_gib=$(awk '/MemTotal/ {printf "%d", $2/1048576}' /proc/meminfo)
info "node $name at $address, ${mem_gib} GiB memory, $(docker --version)"
if $with_model; then
	sudo install -d -m 0755 "$MODELS"
	free_gib=$(df -BG --output=avail "$MODELS" | tail -1 | tr -dc 0-9)
	need=100
	[ -f "$MODELS/$MODEL_ID/.oaf-complete" ] && need=0
	$DOCKER image inspect "$image" >/dev/null 2>&1 || need=$((need + 20))
	[ "$free_gib" -ge "$need" ] || die "$MODELS has ${free_gib} GiB free; the weights and image need about ${need} GiB"
	info "${free_gib} GiB free under $MODELS"
fi

# ---------------------------------------------------------------------------
step "Licences"
if $with_model; then
	cat <<EOF
    Serving $MODEL_ID needs two sets of terms:
      - the vLLM Spark image ships NVIDIA CUDA:      $CUDA_EULA_URL
      - the model is under the NVIDIA Open Model Agreement
        (base model: Qwen Community License 1.0): $MODEL_LICENCE_URL
EOF
	if ! $accept; then
		read -r -p "    Have you read and do you accept both? Type 'yes': " answer
		[ "$answer" = yes ] || die "licences not accepted; nothing installed (or use --no-model)"
	fi
	info "accepted by $email"
fi

# ---------------------------------------------------------------------------
step "oaf binary"
if [ -z "$oaf_bin" ]; then
	oaf_bin=$REPO_ROOT/bin/oaf-linux-arm64
	if command -v go >/dev/null; then
		make -C "$REPO_ROOT" build-arm64 >/dev/null
	else
		info "no Go toolchain; building in $GO_IMAGE"
		$DOCKER run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp \
			-v "$REPO_ROOT":/src -w /src "$GO_IMAGE" make build-arm64 >/dev/null
	fi
fi
[ -x "$oaf_bin" ] || die "no oaf binary at $oaf_bin"
if ! cmp -s "$oaf_bin" /usr/local/bin/oaf 2>/dev/null; then
	sudo install -m 0755 "$oaf_bin" /usr/local/bin/oaf
	agent_changed=true
else
	agent_changed=false
fi
info "$(oaf version | head -1)"

# ---------------------------------------------------------------------------
step "Configuration ($ETC)"
sudo install -d -m 0755 "$ETC" "$ETC/catalog"
sudo install -m 0644 "$REPO_ROOT/examples/catalog.yaml" "$ETC/catalog/models.yaml"
bundle_version=$(sed -n 's/^version: *//p' "$REPO_ROOT/bundle/versions.yaml")
if sudo test -f "$ETC/cluster.yaml" && ! $rewrite_config; then
	info "keeping the existing $ETC/cluster.yaml (--rewrite-config replaces it)"
else
	now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	tmp=$(mktemp)
	{
		cat <<EOF
# Written by scripts/spark-bootstrap.sh on $now. Edit, then run: sudo oaf apply
apiVersion: oaf/v1alpha1
kind: Cluster
metadata:
  name: $name
spec:
  bundle:
    version: $bundle_version
  nodes:
    - name: $name
      address: $address
      roles: [control, gateway, observability, inference]
  fabric:
    topology: single
EOF
		if $with_model; then
			cat <<EOF
  models:
    - name: $DEPLOYMENT
      catalogRef: $MODEL_ID
      placement: { node: $name }
      gpuMemoryUtilization: $util
      maxModelLen: $max_len
EOF
		fi
		cat <<EOF
  routing:
    mode: observe
    gateway:
      port: 4000
    keys:
      - name: default
        secretRef: gateway-key-default
EOF
		if $with_model; then
			cat <<EOF
    defaults:
      fast: $DEPLOYMENT
      fallback: $DEPLOYMENT
EOF
		fi
		cat <<EOF
  observability:
    langfuse: { enabled: true }
    metrics: { retention: 15d }
EOF
		if $with_model; then
			cat <<EOF
  licenseAcceptances:
    - subject: image/$IMAGE_COMPONENT
      url: $CUDA_EULA_URL
      acceptedBy: $email
      acceptedAt: $now
    - subject: model/$MODEL_ID
      url: $MODEL_LICENCE_URL
      acceptedBy: $email
      acceptedAt: $now
EOF
		fi
	} >"$tmp"
	sudo install -m 0644 "$tmp" "$ETC/cluster.yaml"
	rm -f "$tmp"
	info "wrote $ETC/cluster.yaml"
fi
oaf validate -f "$ETC/cluster.yaml" --catalog "$ETC/catalog/models.yaml"

# ---------------------------------------------------------------------------
step "Certificates ($PKI)"
if sudo test -f "$PKI/ca.crt"; then
	info "CA exists; keeping it"
else
	sudo oaf pki init --dir "$PKI" --cluster "$name" --node "$name=$address"
fi
sudo test -f "$PKI/nodes/$name/node.crt" || die "$PKI has no certificate for node $name (was the CA made for another name?)"
sudo install -m 0644 "$PKI/nodes/$name/node.crt" "$PKI/node.crt"
sudo install -m 0600 "$PKI/nodes/$name/node.key" "$PKI/node.key"

# ---------------------------------------------------------------------------
step "Node agent"
unit=/etc/systemd/system/oaf-node.service
if ! cmp -s "$REPO_ROOT/deploy/systemd/oaf-node.service" "$unit"; then
	sudo install -m 0644 "$REPO_ROOT/deploy/systemd/oaf-node.service" "$unit"
	sudo systemctl daemon-reload
	agent_changed=true
fi
sudo systemctl enable --quiet oaf-node
if $agent_changed; then sudo systemctl restart oaf-node; else sudo systemctl start oaf-node; fi
for _ in $(seq 30); do
	systemctl is-active --quiet oaf-node && (exec 3<>/dev/tcp/127.0.0.1/7701) 2>/dev/null && break
	sleep 1
done
systemctl is-active --quiet oaf-node || die "oaf-node is not running: journalctl -u oaf-node -n 50"
info "oaf-node active on :7701"

# ---------------------------------------------------------------------------
if $with_model; then
	step "Engine image"
	if $DOCKER image inspect "$image" >/dev/null 2>&1; then
		info "present: $image"
	else
		info "pulling $image (about 18 GB)"
		$DOCKER pull "$image"
	fi

	step "Weights ($MODELS/$MODEL_ID)"
	dest=$MODELS/$MODEL_ID
	revision=$(awk -v id="$MODEL_ID" '$0 ~ "id: "id"$" {f=1} f && /revision:/ {print $2; exit}' "$ETC/catalog/models.yaml")
	repo=$(awk -v id="$MODEL_ID" '$0 ~ "id: "id"$" {f=1} f && /repo:/ {print $2; exit}' "$ETC/catalog/models.yaml")
	if sudo test -f "$dest/.oaf-complete" && [ "$(sudo cat "$dest/.oaf-complete")" = "$repo@$revision" ]; then
		info "present: $repo@$revision"
	else
		sudo install -d -m 0755 "$dest"
		if [ -n "$weights_from" ]; then
			info "copying from $weights_from"
			# rsync runs as you, so your SSH keys and known_hosts apply. The source
			# may be an HF cache snapshot of symlinks: -L copies the files.
			sudo chown "$(id -u):$(id -g)" "$dest"
			rsync -aL --info=progress2 "${weights_from%/}/" "$dest/"
		else
			info "downloading $repo@$revision (~95 GiB; resumes if interrupted)"
			$DOCKER run --rm --network host -e HF_TOKEN="${HF_TOKEN:-}" -v "$dest":/out \
				--entrypoint python3 "$image" -c "
import sys
from huggingface_hub import snapshot_download
snapshot_download(repo_id=sys.argv[1], revision=sys.argv[2], local_dir='/out', max_workers=8)
" "$repo" "$revision"
		fi
		# Every file the index names must be there before vLLM is pointed at it.
		missing=$(sudo python3 - "$dest" <<'PY'
import json, os, sys
d = sys.argv[1]
idx = json.load(open(os.path.join(d, "model.safetensors.index.json")))
need = sorted(set(idx["weight_map"].values())) + ["config.json", "tokenizer.json"]
print(" ".join(f for f in need if not os.path.isfile(os.path.join(d, f))))
PY
)
		[ -z "$missing" ] || die "the weights are incomplete, missing: $missing (re-run to resume)"
		echo "$repo@$revision" | sudo tee "$dest/.oaf-complete" >/dev/null
		info "complete: $(sudo du -sh "$dest" | cut -f1)"
	fi
fi

# ---------------------------------------------------------------------------
step "Install (preflight, then deploy)"
# Preflight runs inside install, through the agent, and knows the topology:
# on a single node the absent QSFP fabric is skipped, not a failure.
sudo oaf install -f "$ETC/cluster.yaml" --catalog "$ETC/catalog/models.yaml" --timeout 30m

step "Waiting for every project to be healthy (up to $health_timeout min)"
deadline=$(($(date +%s) + health_timeout * 60))
until st=$(sudo oaf status -f "$ETC/cluster.yaml" --catalog "$ETC/catalog/models.yaml" 2>&1); do
	if [ "$(date +%s)" -ge "$deadline" ]; then
		echo "$st"
		die "not healthy after $health_timeout min; see docs/user-guide.md §9 (model logs: docker logs oaf-model-$DEPLOYMENT-vllm-1)"
	fi
	if $with_model; then
		restarts=$($DOCKER inspect -f '{{.RestartCount}}' "oaf-model-$DEPLOYMENT-vllm-1" 2>/dev/null || echo 0)
		if [ "$restarts" -ge 3 ]; then
			$DOCKER logs --tail 30 "oaf-model-$DEPLOYMENT-vllm-1" 2>&1 || true
			die "the model container is crash-looping ($restarts restarts); its last log lines are above"
		fi
		last=$($DOCKER logs --tail 1 "oaf-model-$DEPLOYMENT-vllm-1" 2>&1 | cut -c1-150 || true)
		info "$(date +%H:%M:%S) model: ${last:-starting}"
	fi
	sleep 30
done
echo "$st"

# ---------------------------------------------------------------------------
step "Smoke test through the gateway"
key=$(sudo cat "$ETC/secrets/gateway-key-default")
curl -fsS "http://127.0.0.1:4000/v1/models" -H "Authorization: Bearer $key" >/dev/null && info "GET /v1/models: ok"
if $with_model; then
	start=$(date +%s.%N)
	resp=$(curl -fsS "http://127.0.0.1:4000/v1/chat/completions" -H "Authorization: Bearer $key" \
		-H 'Content-Type: application/json' \
		-d "{\"model\":\"$DEPLOYMENT\",\"max_tokens\":512,\"messages\":[{\"role\":\"user\",\"content\":\"Write a haiku about a small GPU cluster.\"}]}")
	end=$(date +%s.%N)
	python3 - "$resp" "$start" "$end" <<'PY'
import json, sys
r = json.loads(sys.argv[1]); secs = float(sys.argv[3]) - float(sys.argv[2])
n = r.get("usage", {}).get("completion_tokens", 0)
print(f"    POST /v1/chat/completions: {n} tokens in {secs:.1f}s ({n/secs:.1f} tok/s end to end)")
m = r["choices"][0]["message"]
text = (m.get("content") or "").strip() or "(only reasoning within the token limit)"
print("    " + text.replace("\n", "\n    "))
PY
fi

cat <<EOF

$(printf '\033[1m')Done.$(printf '\033[0m') Endpoints on $address:
    OpenAI API   http://$address:4000/v1    key: sudo cat $ETC/secrets/gateway-key-default
    Langfuse     http://$address:3000       admin@oaf.local / sudo cat $ETC/secrets/langfuse-admin-password
    Grafana      http://$address:3001       admin / sudo cat $ETC/secrets/grafana-admin-password
    Prometheus   http://$address:9090
Status: sudo oaf status     Change: sudoedit $ETC/cluster.yaml && sudo oaf apply
Memory left for other apps: about $(awk -v u="$util" -v m="$mem_gib" 'BEGIN {printf "%d", m*(1-u) - 4.6 - 3.1}') GiB (give them compose mem_limit caps).
EOF
