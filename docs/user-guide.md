# openagentfabric user guide

This guide takes you from unboxed GB10 machines (NVIDIA DGX Spark or Asus GX10) to a running,
observable LLM platform, and then covers day-to-day operation and troubleshooting.

> **Status: milestone M1 (CLI only).** The parts below marked *(planned)* do not exist yet. Today
> you place the `oaf` binary and the node agents by hand, address the QSFP fabric yourself and copy
> model weights into place. `oaf install` then does the rest. The one-line installer, the web UI, the
> model download pipeline arrives in a later milestone (see [PLAN.md](PLAN.md) §7).

Contents:
1. [What you get](#1-what-you-get)
2. [Requirements](#2-requirements)
3. [Plan the cluster](#3-plan-the-cluster)
4. [Install, step by step](#4-install-step-by-step)
5. [What to expect](#5-what-to-expect)
6. [Using the platform](#6-using-the-platform)
7. [Models](#7-models)
8. [Day-2 operations](#8-day-2-operations)
9. [Troubleshooting](#9-troubleshooting)
10. [Reference](#10-reference)

---

## 1. What you get

After the install, every node runs a small agent (`oaf-node`). The cluster then runs this stack,
which is all pinned upstream images, unmodified:

| Component | What it does | Runs on |
|---|---|---|
| **agentgateway** | One OpenAI-compatible endpoint for every model. Checks API keys and sends traces to Langfuse | `gateway` node |
| **vLLM** | Serves each model. A model can span a pair of nodes over the QSFP cable | `inference` nodes |
| **Langfuse** (+ Postgres, ClickHouse, Valkey, SeaweedFS) | Traces, latency and token usage of every request | `observability` node |
| **Prometheus + Grafana** | Metrics: nodes, gateway, vLLM | `observability` node |
| **node-exporter** | Host metrics | every node |
| vSR (vLLM Semantic Router) + Envoy | Picks the model for `model: auto` requests (only when `routing.rules` has an active rule) | `gateway` node |
| OAF UI and API (`oafd`) *(planned, M4)* | One web UI for all of the above | `control` node |

Everything is described in one file, `/etc/oaf/cluster.yaml`. You change the cluster by editing
that file and running `oaf apply`. Nothing exists only in a UI or in someone's shell history.

## 2. Requirements

### Hardware
- 1 to 4 GB10 machines (DGX Spark or Asus GX10). The recommended unit is a **pair**: two machines
  joined by one QSFP cable. [architecture.md](architecture.md) §5 explains why.
- For each pair, a **QSFP56/QSFP28 direct-attach cable** between the same-numbered port on both
  machines is simplest (port 1 ↔ port 1). A second cable between the other two ports is allowed.
- A **management network** (ordinary Ethernet or Wi-Fi) on which every machine can reach the others.
  OAF control traffic, the gateway and the web UIs use this network. The QSFP cables carry only
  model traffic between nodes.

There is no QSFP switch. Without one, 4 machines are two separate pairs, not a ring (see §3).

### Software on every node
- **DGX OS** (Ubuntu 24.04 based, tested with kernel 6.17) or the equivalent GX10 image.
- **Docker 29+ with the compose plugin.** On DGX OS, containers get the GPU through **CDI**
  (`/var/run/cdi/nvidia.yaml`). This is the default, so you do not need to register an `nvidia` runtime.
- **perftest** (`ib_write_bw`) for the fabric bandwidth test, and `rdma-core`. Both ship with DGX OS.
- NTP time sync (the default). Traces and certificates need clocks that agree.
- SSH access with sudo for the person installing.

### Resources
- **Disk:** at least 200 GiB free under `/var/lib/oaf`, plus the size of every model placed on the
  node. Preflight warns below 200 GiB.
- **Memory:** GB10 memory is unified, so the CPU and the GPU share 128 GB. The OAF services on the
  observability node are capped at about **8 GiB** in total and use about 2.5 GiB idle. Each model
  takes the share you give it (`gpuMemoryUtilization`). The shares of all models on one node must
  add up to 0.90 or less.

## 3. Plan the cluster

### Topology

| Machines | Topology | Cabling | What runs across nodes |
|---|---|---|---|
| 1 | `single` | none | nothing (every model fits on one node) |
| 2 | `pair` | 1 cable (2 rails), optionally a 2nd cable | one model with tensor parallelism across the pair (TP=2) |
| 3 | `triangle` | 3 cables, each node to both others | pairs inside the triangle; NCCL over sockets |
| 4 | `pairs` | two separate pairs, **no cable between them** | each pair runs its own models |
| 3 | `pair-plus-single` | 1 cable between two nodes; the third is on the LAN only | the pair runs a large model (TP=2), the single node its own models |

**Do not cable 4 machines as a ring.** NCCL needs direct links between nodes that a ring makes
non-adjacent, and RDMA traffic cannot be forwarded through a node. A ring therefore breaks
multi-node inference. `oaf validate` rejects it with an explanation.

### Roles
Each node in `cluster.yaml` has roles:
- `control`: exactly one node. You run `oaf` commands here, and it holds the CA, the secrets and the desired state.
- `gateway`: runs agentgateway (one node in v1).
- `observability`: runs Langfuse, Prometheus and Grafana (exactly one node).
- `inference`: can run models.

For a pair, the usual layout is `[control, gateway, observability, inference]` on the first node and
`[inference]` on the second.

### Ports and cables
Each GB10 has two physical QSFP ports, and the OS shows **each one as two network devices**, one per
PCIe domain:

| Physical port | Rail 0 | Rail 1 |
|---|---|---|
| port 0 | `enp1s0f0np0` | `enP2p1s0f0np0` |
| port 1 | `enp1s0f1np1` | `enP2p1s0f1np1` |

One cable therefore gives you **two rails**, and each rail needs its own small subnet. Plan one `/30`
per rail, for example port 1 of a pair:

| Rail | Node A | Node B | Subnet |
|---|---|---|---|
| 0 (`enp1s0f1np1`) | 192.168.31.2 | 192.168.31.1 | 192.168.31.0/30 |
| 1 (`enP2p1s0f1np1`) | 192.168.41.2 | 192.168.41.1 | 192.168.41.0/30 |

If a port already carries production traffic, leave it alone and cable the pair through the other port.
OAF never reconfigures a port that has addresses it did not assign.

## 4. Install, step by step

Run the steps on the **control node** unless a step says otherwise. Commands that change the
system need `sudo`.

### One box, one command

On a fresh DGX Spark / GX10 (DGX OS, a user with sudo, internet access):

```sh
curl -fsSL https://raw.githubusercontent.com/anindyar/openagentfabric/main/install.sh \
  | bash -s -- --email you@company.com
```

That is the whole platform on this box: it checks the host, checks out OAF into `/opt/oaf/src` and
runs `scripts/spark-bootstrap.sh` (below) with your options. It asks you to accept the NVIDIA and
model licences on the terminal (`--accept-licences` when there is none, e.g. automation).

**With the Garrison security console** on top (the appliance), give it read access to the Garrison
repository in the environment, never on the command line:

```sh
export GARRISON_GIT_TOKEN=<token with read access to the Garrison repository>
curl -fsSL https://raw.githubusercontent.com/anindyar/openagentfabric/main/install.sh \
  | bash -s -- --email you@company.com --garrison
```

After OAF it checks Garrison out into `/opt/garrison`, runs its installer against OAF (Garrison uses
OAF's gateway for its models and embeddings, and OAF's Grafana, Prometheus and Loki), then enrols
this box's own Garrison agent and makes it a log collector, so the appliance shows in Systems and
Observe like every other machine. The token reaches git through an askpass helper (not argv, the
process list or `.git/config`). At the end it prints the console address; the admin password is in
`/opt/garrison/.env` (`ADMIN_PASSWORD`).

| Option | Use |
|---|---|
| `--garrison` | also install the Garrison console (needs `GARRISON_GIT_TOKEN`) |
| `--no-self-enrol` | with `--garrison`: leave this box out of Garrison's systems |
| `--ref REF` | the OAF version (git ref) to install, default `main` |
| `--address IP`, `--name NAME` | management address and node name (defaults: default-route address, hostname) |
| `--no-model`, `--util`, `--weights-from`, ... | passed to `spark-bootstrap.sh` (table below) |

Re-run the same command after a failure: every step checks before it acts. More boxes join later
from the console: §8a "Adding boxes".

### One box from a checkout: `scripts/spark-bootstrap.sh`
For a single GB10 running the whole stack, a chat model and the embedding model, one script does
every step below. The installer above runs it; from a checkout on the
new box:

```sh
git clone https://github.com/anindyar/openagentfabric && cd openagentfabric
scripts/spark-bootstrap.sh --email you@example.com
```

It checks the host (aarch64, docker, compose, the NVIDIA CDI spec, disk), asks you to accept the
licences, builds `oaf` (in a pinned Go container if the box has no Go), writes
`/etc/oaf/cluster.yaml` with `topology: single`, runs preflight, creates the certificates, starts the
node agent, pulls the engine image (~18 GB), downloads the weights (~95 GiB) to
`/var/lib/oaf/models/qwen3.8-flash-next-fp4/` and bge-m3's (~2.3 GiB) to `/var/lib/oaf/models/bge-m3/`,
runs `oaf install`, waits until `oaf status` is healthy and sends a chat and an embedding request
through the gateway. On a box installed before bge-m3 was a default, a re-run adds it to cluster.yaml,
but only when at least 9 GiB of memory is available. A first run takes about 30–60 minutes, mostly the
download and the model's first start (it autotunes kernels for about 10 minutes; the results are kept
in volumes, so later starts are faster). Every step checks before it acts, so re-run it after a failure.

| Option | Default | Use |
|---|---|---|
| `--util` | `0.75` | vLLM's share of unified memory. 0.75 keeps the full 262K context with ~800K tokens of KV cache and, with bge-m3, leaves about 15 GiB for other apps |
| `--weights-from user@host:/path/` | Hugging Face | copy the weights from another box over SSH instead of downloading |
| `--no-model` | | the stack only |
| `--no-embeddings` | | no bge-m3 embedding model (frees about 6.5 GiB) |
| `--name`, `--address` | hostname, default-route address | the node name (a DNS label) and management address |
| `--rewrite-config` | keep | replace an existing `/etc/oaf/cluster.yaml` |
| `--accept-licences` | prompt | accept without the prompt (re-runs, automation) |

The model is Qwen3.8 Flash-Next (NVIDIA NVFP4). Upstream vLLM cannot load its checkpoint yet, so the
catalog serves it with the community GB10 build `eugr/spark-vllm-b12x`, pinned by digest in
`bundle/versions.yaml` (`engine: vllm-spark-b12x` in the catalog profile). Other workloads on the same
box should get compose `mem_limit` caps so they cannot push vLLM into the OOM killer.

For pairs and triangles, follow the steps below.

### Step 1: get the binary
Build it on any machine with Go 1.27 (the result is one static arm64 file):

```sh
git clone https://github.com/anindyar/openagentfabric && cd openagentfabric
make build                          # → bin/oaf-linux-arm64
scp bin/oaf-linux-arm64 user@<each node>:/tmp/oaf
```

On every node:

```sh
sudo install -m 0755 /tmp/oaf /usr/local/bin/oaf
oaf version
```

*(planned, M5: `curl -fsSL https://openagentfabric.dev/install.sh | bash` does this and every step below.)*

### Step 2: the fabric addresses (pairs and triangles)
OAF addresses the rails itself. Give every link in `fabric.links` (Step 3) one subnet per rail; the
console's topology planner fills them in from 10.200.0.0/16:

```yaml
links:
  - { a: { node: spark-1, port: 1 }, b: { node: spark-2, port: 1 }, subnets: [10.200.0.0/30, 10.200.0.4/30] }
```

End A of each rail gets the first host of its subnet, end B the second. `oaf apply` and `oaf install`
then, after the desktop mode and before any project:

1. write `/etc/netplan/40-oaf-fabric.yaml` (mode 600) on every node of a link: the rails with MTU 9000
   and no IPv6 link-local addresses. `netplan apply` runs only when the file or the live addresses
   differ, and OAF waits for the addresses;
2. ping every rail, and again at the full MTU with fragmentation off. A rail that fails stops the apply;
3. on a link they just brought up, run `ib_write_bw` on each rail and on both rails together (as in
   Step 7). A slow rail is a warning.

```
updated   spark-1/fabric  enp1s0f1np1 10.200.0.1/30, enP2p1s0f1np1 10.200.0.5/30
updated   spark-2/fabric  enp1s0f1np1 10.200.0.2/30, enP2p1s0f1np1 10.200.0.6/30
link  spark-1:enp1s0f1np1 ↔ spark-2:enp1s0f1np1  0.26 ms, MTU 9000 ok
link  spark-1:enP2p1s0f1np1 ↔ spark-2:enP2p1s0f1np1  0.53 ms, MTU 9000 ok
spark-1:1 ↔ spark-2:1  rail 0  10.200.0.1 → 10.200.0.2  104.6 Gb/s
spark-1:1 ↔ spark-2:1  rail 1  10.200.0.5 → 10.200.0.6  111.6 Gb/s
spark-1:1 ↔ spark-2:1  2 rails together  189.4 Gb/s
```

`oaf apply --dry-run` prints the `update` lines without changing anything. Notes:

- OAF writes only that one file. It refuses a port whose netdev carries an address OAF did not assign
  (another netplan file, a NetworkManager profile, an address added by hand) and names it: remove
  the address or leave the port out of `fabric.links`. A file already at that path, as written by hand
  below, is taken over. Dropping a link from cluster.yaml removes its rails from the file.
- The first `netplan apply` briefly takes the rails down. Do not list a port that carries production
  traffic.
- The node agent writes the file, so its unit allows `/etc/netplan` (`ReadWritePaths=-/etc/netplan`
  in `deploy/systemd/oaf-node.service`). An agent installed with an older unit cannot: install the new
  unit and run `systemctl daemon-reload` and `systemctl restart oaf-node`.
- `link-local: []` keeps IPv6 link-local addresses off the rails. OAF detects the RoCE GID index on
  every run anyway, because the index moves when addresses are added, but this keeps the GID table
  small and predictable.

**Fallback: addressing by hand.** A link without `subnets` is yours: OAF writes and removes nothing on
its nodes and only checks it (Step 7). Put this in `/etc/netplan/40-oaf-fabric.yaml` on node A, using
your free port and the plan from §3:

```yaml
network:
  version: 2
  renderer: NetworkManager
  ethernets:
    enp1s0f1np1:   {dhcp4: false, dhcp6: false, link-local: [], mtu: 9000, addresses: [192.168.31.2/30]}
    enP2p1s0f1np1: {dhcp4: false, dhcp6: false, link-local: [], mtu: 9000, addresses: [192.168.41.2/30]}
```

Use the `.1` addresses on node B, with **MTU 9000 on both ends** (the fabric check fails without it).
Then apply it and check it on each node:

```sh
sudo chmod 600 /etc/netplan/40-oaf-fabric.yaml
sudo netplan apply
ip -br addr show | grep -E 'enp1s0f1|enP2p1s0f1'     # both UP with their address
ping -c1 192.168.31.1 && ping -c1 192.168.41.1       # from node A
```

### Step 3: write cluster.yaml
Start from the example and edit the node names, addresses and links:

```sh
sudo install -d /etc/oaf/catalog
sudo cp examples/cluster.pair.yaml /etc/oaf/cluster.yaml
sudo cp examples/catalog.yaml /etc/oaf/catalog/models.yaml
sudoedit /etc/oaf/cluster.yaml
```

The parts you will change:

```yaml
spec:
  nodes:
    - name: spark-1                  # a DNS-label name, used in certificates
      address: 192.168.0.11          # management address (not a fabric IP)
      roles: [control, gateway, observability, inference]
    - name: spark-2
      address: 192.168.0.12
      roles: [inference]
  fabric:
    topology: pair
    mtu: 9000
    links:                           # one entry per cable
      - a: { node: spark-1, port: 1 }
        b: { node: spark-2, port: 1 }
        subnets: [192.168.31.0/30, 192.168.41.0/30]   # rail 0, rail 1 (optional; picks the address if a rail has several)
    roce: { gidIndex: auto }         # detected per run; do not hard-code
    nccl: { transport: auto }        # RoCE on a pair, sockets on a triangle
  models: [...]                      # see §7
  routing:
    gateway: { port: 4000 }
    keys:
      - { name: default, secretRef: gateway-key-default }   # the key is generated at install
```

Then validate:

```sh
oaf validate        # schema + semantic checks: topology, ports, memory budget, licences, routing targets
```

`validate` explains every problem, for example a 4-node ring, two models on one node whose shares add
up to more than 0.90, a routing rule that points at a model that is not deployed, or a licence that
has not been accepted (§7).

### Step 4: preflight (read-only)
```sh
oaf preflight                                   # this node
oaf preflight --ssh user@192.168.0.12           # another node, over SSH
```

Preflight changes nothing. It runs only an allowlist of read-only commands, so it is safe on
production machines. It checks the host, GPU, memory, disk, docker, GPU access for containers,
existing workloads, ports, the QSFP fabric, RDMA tools, NCCL and time sync. `FAIL` blocks the install.
`WARN` is information you should read (§9.1).

### Step 5: certificates
```sh
sudo oaf pki init --dir /etc/oaf/pki --cluster <cluster name> \
  --node spark-1=192.168.0.11 --node spark-2=192.168.0.12
```

This writes the cluster CA, the client certificate that `oaf` commands use (`oafd.crt`), and one
certificate per node under `/etc/oaf/pki/nodes/<name>/`. `ca.key` must stay on the control node only.
`pki init` refuses to overwrite an existing CA.

### Step 6: the node agent on every node
Copy `ca.crt`, `node.crt` and `node.key` from `/etc/oaf/pki/nodes/<name>/` to each node, then on that
node (the control node included):

```sh
sudo install -d -m 0700 /etc/oaf/pki
sudo install -m 0644 ca.crt node.crt /etc/oaf/pki/
sudo install -m 0600 node.key /etc/oaf/pki/
sudo install -m 0644 deploy/systemd/oaf-node.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now oaf-node
systemctl is-active oaf-node                       # → active
```

On the control node, the node files go next to the CA:
`sudo cp /etc/oaf/pki/nodes/spark-1/node.{crt,key} /etc/oaf/pki/`.

The agent listens on TCP 7701 and accepts only this cluster's `oafd` certificate. It manages only
compose projects named `oaf-*`, so it never touches your other containers.

*(planned: `oaf install` will push the agent over SSH.)*

### Step 7: check the fabric
```sh
oaf fabric check
```

For every cable this checks that both ends have carrier, MTU 9000 and their rail addresses in one
subnet, and that a RoCE v2 GID exists for each rail address. It then runs `ib_write_bw` on each rail
alone and on both rails together, and compares the results with `fabric.acceptance` (defaults: 85 Gb/s
per rail, 170 Gb/s together). The bandwidth runs load the link for about 20 seconds in total.
`--bandwidth=false` checks only the configuration. Before the agents are installed you can run it
over SSH: `--ssh spark-1=user@host --ssh spark-2=user@host`.

Output on a healthy pair:

```
spark-1:1 ↔ spark-2:1  rail 0  192.168.31.2 → 192.168.31.1  111.6 Gb/s
spark-1:1 ↔ spark-2:1  rail 1  192.168.41.2 → 192.168.41.1  111.6 Gb/s
spark-1:1 ↔ spark-2:1  2 rails together  196.0 Gb/s
PASS  spark-1:1 ↔ spark-2:1  2 rails, 196.0 Gb/s together (min 85 per rail, 170 together)
```

### Step 8: install
```sh
sudo oaf install
```

`install` runs preflight on every node through the agents, then the fabric check, and stops if either
fails (`--force` continues anyway). It then generates the secrets, renders every component for every
node, prints the plan and applies it in dependency order: node-exporter, Langfuse, metrics, models,
gateway.

```sh
oaf status            # health of every project; exits non-zero until everything is healthy
```

## 5. What to expect

**Timing.** The first install pulls about 3–4 GB of images for the services, plus the vLLM image if you
deploy models. On a typical connection that takes a few minutes. A reinstall with the images present
takes about 20 seconds. Langfuse web needs about a minute to run its migrations and become healthy.
**Models take much longer:** vLLM loads the weights and captures CUDA graphs, which takes 5–20 minutes
for large models. Their health check allows 30 minutes.

**Install output** (single control node, no models):

```
WARN  threeofnine (threeofnine)
      WARN  memory         9.2 GiB available of 121.6 GiB unified memory
      WARN  workloads      2 existing inference container(s) share the GPU and unified memory; OAF will not touch them
create    threeofnine/node-exporter
create    threeofnine/langfuse
create    threeofnine/metrics
create    threeofnine/gateway
4 change(s), 0 unchanged

created   threeofnine/langfuse  clickhouse=running/healthy langfuse-web=running/starting ...
applied; run `oaf status` to follow health (models can take many minutes to load)
```

**On disk:**

| Path | Contents |
|---|---|
| `/etc/oaf/cluster.yaml`, `/etc/oaf/catalog/` | desired state (yours to edit) |
| `/etc/oaf/pki/` | CA and certificates |
| `/etc/oaf/secrets/` | generated passwords and keys, one file each, mode 0600, never overwritten |
| `/var/lib/oaf/projects/<name>/` | rendered compose projects (generated: do not edit) |
| `/var/lib/oaf/models/<catalog id>/` | model weights |
| docker volumes `oaf-langfuse_*`, `oaf-metrics_*` | traces, metrics, dashboards |

**Containers** are named `oaf-<project>-<service>-1`, for example `oaf-langfuse-clickhouse-1`. Each
has a memory limit and bounded logs (3 × 50 MB), and restarts automatically after a crash or reboot.

## 6. Using the platform

### Endpoints (on the gateway and observability node)

| Service | URL | Login |
|---|---|---|
| OpenAI-compatible API | `http://<gateway>:4000/v1` | `Authorization: Bearer <gateway key>` |
| Langfuse | `http://<observability>:3000` | `admin@oaf.local` / `langfuse-admin-password` |
| Grafana | `http://<observability>:3001`: opens on the **OAF overview** dashboard; folder **OAF** also has models (vLLM), gateway and nodes | `admin` / `grafana-admin-password` |
| Prometheus | `http://<observability>:9090` | none (keep it on a trusted network) |
| Loki (if `observability.logs.enabled`) | `http://<observability>:3100` | none (keep it on a trusted network) |
| agentgateway admin UI | `http://127.0.0.1:15000` on the gateway node only | none (SSH tunnel: `ssh -L 15000:127.0.0.1:15000 <gateway>`) |

Read a credential on the control node with `sudo cat /etc/oaf/secrets/<name>`.

### First request
```sh
KEY=$(sudo cat /etc/oaf/secrets/gateway-key-default)
curl http://<gateway>:4000/v1/models -H "Authorization: Bearer $KEY"
curl http://<gateway>:4000/v1/chat/completions -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"fast","messages":[{"role":"user","content":"Hello"}]}'
```

`model` is the deployment name from `cluster.yaml`. `model: auto` is routed by vSR (see Routing
below), or goes to `routing.defaults.fast` when no routing rule is active. Every request shows up in Langfuse (project named after the cluster) within
a few seconds, with latency and token counts. Prompt and response text are recorded only with
`observability.langfuse.captureContent: true`.

Any OpenAI SDK works: set the base URL to `http://<gateway>:4000/v1` and the API key to the gateway key.

### Routing: `model: auto`

A request with `"model": "auto"` lets OAF pick the model: simple requests stay on the fast model,
complex ones go to a bigger one. Explicit model names are never re-routed.

```yaml
spec:
  routing:
    mode: observe          # observe first; switch to active once the decisions look right
    defaults:
      fast: qwen-flash     # everything a rule does not claim, and where requests go if vSR is down
      fallback: qwen-flash # used when the model a rule picked is down
    rules:
      - name: complex
        target: glm        # must be an enabled chat deployment
        match:
          signals:         # the rule fires if ANY signal fires
            - { type: keyword, keywords: [step by step, prove, think carefully] }
            - { type: context, minTokens: 4000 }   # long inputs
            - { type: reasoning, threshold: 0.1 }  # vSR's complexity score (needs the encoder, below)
```

- **Signals:** `keyword` and `context` need nothing extra. `reasoning` compares the prompt with
  built-in hard and easy examples (infrastructure and security work) using vSR's encoder: about
  1.2 GB on disk and 1.75 GiB of memory on the gateway node, counted in that node's memory budget.
  `domain` signals are accepted but skipped with a warning until a domain classifier is pinned.
- **Observe:** vSR decides but every request goes to `defaults.fast`. Each model call's trace in
  Langfuse carries `oaf.route` (the rule that fired, or `default`), `oaf.routed_model` (vSR's pick)
  and `oaf.served_by`. When the decisions look right, set `mode: active` and `oaf apply`.
- **Failures:** a model that fails its health check is replaced by `defaults.fallback`; with vSR
  itself down, `auto` goes to `defaults.fast`. `oaf.served_by` shows what actually answered.
- **Shape:** with at least one active rule, the gateway node runs a `router` project: vSR and Envoy
  on port 8899. agentgateway sends `auto` there; nothing else changes. Without an active rule there
  is no router.
- **The encoder (reasoning signals only):** OAF does not fetch it yet; put the pinned files on the
  gateway node before `oaf apply` (`bundle/versions.yaml`, `artifacts`, holds the revision):

  ```sh
  sudo docker run --rm --network host -v /var/lib/oaf/models/vsr-encoder:/out python:3.12-slim sh -c \
    'pip -q install huggingface_hub && python -c "from huggingface_hub import snapshot_download as s; s(\"llm-semantic-router/Vela-1.0-Encoder-307M-Embedding\", revision=\"1e57cebf5a7b7fec6e6973f05bbca97c5cca4436\", local_dir=\"/out\", allow_patterns=[\"config.json\",\"modules.json\",\"model.safetensors\",\"special_tokens_map.json\",\"tokenizer.json\",\"tokenizer_config.json\",\"1_Pooling/*\"])"'
  ```

  Without it vSR does not start, Envoy lets requests through, and `auto` goes to `defaults.fast`.

### Metrics and logs for apps

Apps installed next to OAF use its Prometheus, Grafana and (optionally) Loki instead of running
their own. Three settings, all off by default:

```yaml
spec:
  observability:
    metrics: { remoteWrite: true }      # Prometheus accepts pushes on /api/v1/write
    logs: { enabled: true, retention: 7d }  # Loki on :3100, with a Grafana datasource
    grafana: { anonymousViewer: true }  # view dashboards without signing in, for embedding
```

Or, keeping the rest of the file as it is: `sudo oaf observability enable logs remote-write
anonymous-grafana`, then `sudo oaf apply`.

An app that embeds Grafana from its own origin (one certificate for the browser to trust) proxies
a path to it and tells Grafana: `sudo oaf observability grafana-url https://<app>/grafana/`. Grafana
then serves from `/grafana/`, on port 3001 too (`http://<observability>:3001/grafana/`).

An app's own `/metrics` joins with `sudo oaf observability scrape <app> https://<host>:<port>/metrics
[--insecure]` (job `app-<app>`, label `app`; `--insecure` accepts the app's self-signed certificate).

An app adds dashboards by writing JSON files to `/var/lib/oaf/grafana/apps/<app>/`; Grafana shows
each subdirectory as a folder and picks up changes within 30 seconds.

Prometheus and Loki have no authentication: anyone who reaches ports 9090 and 3100 can read every
metric and log line, and push their own. By default they (and Grafana) are published on every
address of the observability node. An app that reaches them only from containers on the same host
binds them to the docker bridge instead, so the LAN cannot reach them:

```sh
sudo oaf observability bind 172.17.0.1    # the docker0 address; "all" restores the default
sudo oaf apply
```

Containers reach them at `host.docker.internal` (`extra_hosts: host.docker.internal:host-gateway`).
Remote collectors then push through an authenticating proxy (Garrison's endpoint collectors push
through its own TLS endpoint).

### Apps: their own key, rotated by the app

Give each app that calls models its own key rather than sharing `default`, so its traffic is
labelled in Langfuse and its key can be rotated or revoked on its own:

```sh
sudo oaf keys issue vclocal --host host.docker.internal --env-out /tmp/vclocal-gateway.env
```

`issue` adds `{name: vclocal, secretRef: gateway-key-vclocal}` to `routing.keys` in cluster.yaml
(comments are kept), generates the key, applies the gateway, and writes the app's settings:

```sh
AGENT_GATEWAY_URL=http://host.docker.internal:4000/v1
AGENT_GATEWAY_API_KEY=<64 hex>
AGENT_GATEWAY_ROTATE_URL=http://host.docker.internal:7702/v1/keys/rotate
AGENT_GATEWAY_MODEL=auto
AGENT_GATEWAY_CONTEXT_WINDOW=<smallest maxModelLen of the enabled models>
```

It is idempotent: an installer can run it every time and gets the same key back. `--host` is the
gateway as the app sees it (`host.docker.internal` for a container on the gateway node; the default
is the gateway node's address). `--models a,b` limits the key to those models.

The app rotates its key itself by posting to the key service (`oaf-keys.service`, port 7702) with
its current key; the answer carries the new key, and the old one stops working:

```sh
curl -X POST http://<control>:7702/v1/keys/rotate -H "Authorization: Bearer $CURRENT_KEY"
# {"name":"vclocal","key":"<new key>"}
```

An admin can do the same with `sudo oaf keys rotate <name>`; `sudo oaf keys list` shows the keys
(never their values). Rotating re-renders the gateway, which restarts to pick the key up: requests
in flight at that moment fail and should be retried. A key can be rotated at most every 30 seconds.
If the gateway cannot be applied, the old key is put back and stays valid.

**Who made a request.** An app can name the end user and conversation behind each call with the
`X-OAF-User` and `X-OAF-Session` headers. Langfuse then lists the trace under that user and session.
The headers only label traces; they grant nothing, and requests without them are traced as before.

**Which step used which model.** Each call is traced as `llm <model>`. An app names the step that
made it with `X-OAF-Step` (e.g. `chat pass 2`, `case draft`), shown as the `oaf.step` attribute.
To see its own work and the model calls in one trace, the app sends its spans to Langfuse
(`$LANGFUSE_HOST/api/public/otel/v1/traces`) and passes a W3C `traceparent` header on each call:
the gateway exports the call as a child of the app's span. `oaf keys issue` prints `LANGFUSE_HOST`,
`LANGFUSE_PUBLIC_KEY` and `LANGFUSE_SECRET_KEY` for this when Langfuse is on; the same keys read
traces back through Langfuse's public API.

### External models

`spec.routing.providers` serves external models by name next to the local ones: `openai`,
`anthropic`, `gemini`, `azure-openai` or any `openai-compatible` endpoint.

```yaml
spec:
  routing:
    providers:
      - name: openai
        type: openai
        secretRef: provider-openai   # the API key; never generated
        allowEgress: true            # required: prompts leave the appliance
        guardrails: [basic]
        models:
          - name: gpt-4o             # what clients send as `model`
            upstream: gpt-4o-2024-08-06
      - name: lab
        type: openai-compatible
        endpoint: http://llm.example.internal:8010/v1   # no key needed: omit secretRef
        allowEgress: true
        guardrails: [basic]
        models: [{ name: lab-model }]
```

Store the key, then apply: `printf %s "$OPENAI_API_KEY" | sudo oaf secrets set provider-openai`
and `sudo oaf apply`. Keys reach the gateway through its env file, never through config.yaml.
`azure-openai` takes `azure: { resourceName, apiVersion }` instead of an endpoint.

**A provider without guardrails is allowed, and warned about on every validate and plan:**
*External model without guardrails: prompts, code and data leave the appliance unfiltered. This is
extremely dangerous; use with care.* Start with the `basic` policy below. External models are
reached by explicit model name; `model: auto` uses one only when a routing rule names it.

### Guardrails

`spec.routing.guardrails` are named policies of agentgateway prompt guards, attached to providers
(`providers[].guardrails`) and to local deployments (`models[].guardrails`; `auto` follows the
fast model's).

```yaml
spec:
  routing:
    guardrails:
      - name: basic
        description: Recommended minimum for external models.
        message: "Blocked by the appliance's guardrails: the request contains a credential or private key."
        request:                     # prompts, before the model sees them
          - regex:
              action: reject         # reject | mask | audit
              patterns:
                - 'AKIA[0-9A-Z]{16}'                    # AWS access key id
                - '-----BEGIN [A-Z ]*PRIVATE KEY-----'  # PEM private key
                - 'gh[pousr]_[A-Za-z0-9]{36,}'          # GitHub token
                - 'sk-[A-Za-z0-9_-]{20,}'               # OpenAI-style key
                - 'xox[abprs]-[A-Za-z0-9-]{10,}'        # Slack token
                - '[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:[^/\s@]+@'   # password in a URL
                - '(?i)(password|passwd|secret|api[_-]?key)\s*[:=]\s*\S{6,}'
        response:                    # model output, before the client sees it
          - regex:
              action: mask
              builtins: [email, phoneNumber, creditCard]
```

Built-in detectors are those of the pinned agentgateway: `ssn`, `creditCard`, `phoneNumber`,
`email`, `caSin`; any other name fails validation. Custom patterns must compile. A rejected request
gets HTTP 403 with the policy's `message`; masked values come back as placeholders such as
`<EMAIL_ADDRESS>`. A guard can also call an external service: `webhook: { host: guard:9000 }`.

### Management API (oafd)

`oaf serve` (`oaf-api.service`, port 8443 on the control node, installed by the bootstrap) is the
API an app uses to run the platform: everything above, without editing files. It never keeps state
of its own: every change is a **plan**, a diff of cluster.yaml with its validation warnings and
errors, applied in a second step.

```sh
sudo oaf keys issue garrison --admin --host host.docker.internal
# ... AGENT_GATEWAY_* and LANGFUSE_* as above, plus:
# OAF_API_URL=https://host.docker.internal:8443
# OAF_API_TOKEN=<management token, secret oaf-admin-garrison>
# OAF_API_CERT_SHA256=<fingerprint of the API's certificate, to pin>
```

| Method | Path (`/api/v1/...`) | |
|---|---|---|
| GET | `status`, `config`, `catalog` | nodes, deployments and components; cluster.yaml and the rendered gateway config; the catalog |
| GET, POST · DELETE | `models` · `models/{name}`; PUT `models/{name}/guardrails` | local deployments |
| GET, POST · PUT, DELETE | `providers` · `providers/{name}` | external providers; `api_key` is stored once and never returned |
| GET, POST · PUT, DELETE | `guardrails` · `guardrails/{name}` | policies (`{"template":"basic"}`, `attach: {providers, models}`) |
| GET, PUT | `routing` | mode, defaults, rules |
| GET, POST · POST | `keys` · `keys/{name}/rotate` | app keys (the new key is in that reply only) |
| PUT | `nodes/{name}/headless` | `{"headless": true|false}`: run the box without its desktop GUI, or bring it back (a plan) |
| GET · POST | `plans/{id}` · `plans/{id}/apply` | follow a plan; apply it (409 if cluster.yaml changed since) |
| GET | `events` | server-sent events: plan progress |

Writes answer `201` with `{plan_id, summary, diff, warnings, errors}`, or `422` when the change does
not validate (such a plan cannot be applied). Requests need `Authorization: Bearer $OAF_API_TOKEN`.

## 7. Models

Each entry under `spec.models` is one served model:

```yaml
  models:
    - name: fast                                   # the name clients send as "model"
      catalogRef: qwen3-30b-a3b-instruct-2507-fp8  # an id in catalog/models.yaml
      placement: { node: spark-1 }                 # one node...
      gpuMemoryUtilization: 0.30                   # share of the node's 128 GB (weights + KV cache)
      maxModelLen: 32768
    - name: think
      catalogRef: gpt-oss-120b
      profile: pair-tp2                            # catalog profile: 2 nodes, tensor parallel 2
      placement: { group: pair-a }                 # ...or a group of nodes joined by a cable
      gpuMemoryUtilization: 0.55
```

- **Weights (until M2):** copy the Hugging Face snapshot of the pinned revision to
  `/var/lib/oaf/models/<catalog id>/` on **every** node of the placement. On a pair, download once
  and copy it over the fabric:
  `rsync -a /var/lib/oaf/models/gpt-oss-120b/ 192.168.31.1:/var/lib/oaf/models/gpt-oss-120b/`.
  vLLM runs with `HF_HUB_OFFLINE=1` and never downloads anything itself.
- **Memory shares:** vLLM takes about 90% of unified memory unless told otherwise, so every
  deployment states its share. The shares on one node must add up to 0.90 or less, which leaves room
  for the OS and the OAF services. Each vLLM instance also uses about 4.6 GiB outside its share
  (CUDA context and processes); `oaf validate` warns when the shares plus that overhead pass 0.90.
- **Embeddings:** catalog models of class `embedding` serve `/v1/embeddings` through the gateway, by
  deployment name, and are never a chat route (`routing.defaults` and rules refuse them). The
  one-box appliance serves `bge-m3` (1024 dimensions) by default, at `gpuMemoryUtilization: 0.03`:
  about 6.5 GiB in all, measured on a GB10, nearly all of it vLLM's fixed overhead.

  ```sh
  curl http://<gateway>:4000/v1/embeddings -H "Authorization: Bearer $KEY" \
    -H 'Content-Type: application/json' -d '{"model":"bge-m3","input":"firewall rule review"}'
  ```

  `oaf keys issue` passes it to apps as `AGENT_GATEWAY_EMBEDDING_MODEL` and
  `AGENT_GATEWAY_EMBEDDING_DIMENSIONS`. To add it to an existing cluster.yaml (comments kept,
  validated before it is written): `sudo oaf models add bge-m3 --catalog-ref bge-m3`, then
  `sudo oaf apply`; the bootstrap does this itself when enough memory is free.
- **Licences:** components or models whose terms are not plain open source must be accepted first. Read
  the page that `oaf validate` names and record it under `spec.licenseAcceptances`. The vLLM image
  ships NVIDIA's CUDA libraries, so it needs `image/vllm`:

  ```yaml
  licenseAcceptances:
    - subject: image/vllm
      url: https://docs.nvidia.com/cuda/eula/index.html
      acceptedBy: you@example.com
      acceptedAt: 2026-10-17T12:00:00Z
  ```

- **Engines:** a catalog profile's `engine` names the bundle component whose image serves it: `vllm`
  (upstream, the default) or a pinned GB10 build such as `vllm-spark-b12x`. Its `container` block
  carries what that image's documented run command needs (`ipc: host`, `shmSize`, `ulimits`,
  `securityOpt`) and the cache paths kept in named volumes. Each engine image with NVIDIA terms needs
  its own `image/<component>` acceptance.
- **Disabling** a model (`enabled: false`) and running `oaf apply` stops and removes it on every node.
  Its weights stay on disk.

*(planned, M2: `oaf models search/pull` downloads from Hugging Face, fans out over the fabric, checks
disk and checksums, and benchmarks each model against its catalog baseline.)*

## 8. Day-2 operations

| Task | Command |
|---|---|
| See what a change would do | `oaf apply --dry-run` (changes nothing and generates no secrets) |
| Apply a change to `cluster.yaml` | `sudo oaf apply` |
| Health of everything | `oaf status` |
| Re-check the fabric | `oaf fabric check` |
| Logs of a project | `sudo docker compose -p oaf-langfuse logs -f --tail 100` |
| Remove the stack, keep data | `sudo oaf uninstall --yes` |
| Remove the stack and its data | `sudo oaf uninstall --yes --purge` (deletes all traces, metrics and dashboards) |

- `apply` is always safe to re-run. Unchanged projects are only converged, which also restarts
  anything that stopped. Projects that are no longer in `cluster.yaml` are removed (their volumes are kept).
- **Back up** `/etc/oaf` (desired state, CA, secrets) and, if you need history, the `oaf-langfuse_*`
  volumes. If you lose `/etc/oaf/secrets`, the existing Langfuse data cannot be decrypted with new secrets.
- `uninstall` leaves the weights, secrets, certificates and agents in place. To remove an agent:
  `sudo systemctl disable --now oaf-node && sudo rm /etc/systemd/system/oaf-node.service && sudo rm -r /var/lib/oaf/projects`.
- **Upgrades** *(planned, M5)*: a new OAF release bumps the pinned image digests in the bundle, and `oaf apply` rolls them out.

## 8a. Adding boxes

OAF grows in pairs of two (there is no switch). From a single box there are three ways to grow;
the console (or `POST /api/v1/topology/recommend`) shows which are possible for the boxes you
have, what then fits, and how queries are routed.

**a) +1 box over ConnectX-7: a pair.** This box and the new one are joined by one QSFP cable
(two rails, ~185 Gbps). Models too large for one box run across both (TP=2). This box's own
models stay, so a pair model needs room next to them on this box.

```
  ┌─────────┐ port 1 ══QSFP══ port 1 ┌─────────┐
  │ spark-1 │                        │ spark-2 │      topology: pair
  └────┬────┘                        └────┬────┘
       └───────────── LAN ───────────────┘
```

**b) +1 box over the LAN: high availability (1:1).** A second complete appliance with the same
models takes over when this one fails; users fail over through an external load balancer.
*Planned (ADR-0002 §6); the console describes it but cannot set it up yet.*

**c) +1 pair: two new boxes cabled to each other.** This box keeps the stack and a small model;
the pair runs a large model such as GLM-5.3-Flash (~190 GiB, TP=2); `model: auto` sends complex
queries to the pair and simple ones stay local. Only the pair is cabled with QSFP.

```
  ┌─────────┐                ┌─────────┐ port 1 ══QSFP══ port 1 ┌─────────┐
  │ spark-1 │                │ spark-2 │                        │ spark-3 │
  └────┬────┘                └────┬────┘                        └────┬────┘
       └─────────────────── LAN ──┴──────────────────────────────────┘
                                         topology: pair-plus-single
```

Without a probe result (below), port 1 is used: port 0 is often already in use. When the probe
finds both ports of a pair cabled to each other, the plan uses both cables: one link each, two rails
each, four rails in all. A port that already carries an address configured by something else is
never chosen.

**Joining a box.** Nothing is deployed when a box joins; it waits until you choose a topology.

1. In the console, choose how many boxes you add (or `POST /api/v1/nodes/join-tokens
   {"count": 2}`). You get one command, valid for an hour and for that many boxes:

   ```sh
   export OAF_JOIN_TOKEN=oafj_…; curl -fsSLk --pinnedpubkey 'sha256//…' https://<control>:8443/join.sh | sudo --preserve-env=OAF_JOIN_TOKEN bash -s
   ```

   The pin is the control node's TLS key: the script refuses anything else on that address. The
   token travels in the environment (and to curl on stdin), so it never appears in a command line
   that other users can see in the process list.
2. Run it on each new box (DGX OS, docker present). It downloads `oaf` and the bundle images from
   the control node (no internet needed; `--skip-images` defers the images to deploy time), runs the
   read-only preflight, enrols in the cluster PKI, and starts `oaf-node`. A box that fails a blocking
   check (wrong architecture, no GPU, no docker or GPU runtime) does not join; `--force` overrides.
3. The box shows up as **pending** with its hardware, preflight and both QSFP ports.
4. The console asks for a recommendation, and the control node probes the cabling of the pending
   boxes: each up QSFP netdev that is free gets a temporary address from 198.18.0.0/24 (a range
   reserved for benchmarking, never routed), pings the other boxes' probe addresses, and the MAC
   that answers names the port at the other end. The addresses are removed straight after (about
   5 seconds; cached for two minutes). Cables it found are shown as done.
5. Choose the option. The plan (a `cluster.yaml` diff: nodes, one link per cable with one /30 per
   rail from 10.200.0.0/16, a group, the large model and its routing rule) is reviewed, then
   applied: fabric bring-up and check (§4 Step 2), weights copied over the fabric, deploy,
   acceptance.

## 8b. Headless boxes (no desktop GUI)

DGX OS boots into the GNOME desktop. On GB10 the GPU memory is the system memory, so the desktop
(gdm, Xorg, gnome-shell and the session's services) holds memory a model could use: about 1.8 GiB
measured on a fresh DGX Spark with a user logged in, more with apps open. To give it back:

```yaml
  nodes:
    - name: spark-2
      address: 192.0.2.12
      roles: [inference]
      headless: true      # false brings the desktop back; unset leaves the box as it is
```

`oaf apply` then switches the box through its agent, before any model starts:
`systemctl set-default multi-user.target` and `systemctl stop display-manager` (headless), or
`graphical.target` and `start` (desktop). It never reboots, it is idempotent, and SSH, the node agent
and the console are unaffected; only the local screen stops showing a desktop, and anyone logged in
there loses their session (the apply warns). `GET /api/v1/nodes` shows each box's `desktop`
(`mode`, `display_manager_active`, `estimated_mib`: the display stack's resident memory, an estimate
from `/proc`, since `nvidia-smi` reports no per-process memory on unified memory). The Add boxes
plan can set new boxes headless (`"headless": true` on `POST /api/v1/topology/plan`).

## 9. Troubleshooting

Start with `oaf status`. It names the node, the project and the state of each container. For a
project that is not healthy, read its logs:

```sh
sudo docker compose -p oaf-<project> ps
sudo docker compose -p oaf-<project> logs --tail 100 <service>
sudo journalctl -u oaf-node -n 50          # the agent itself
```

### 9.1 Preflight warnings and failures

| Message | Meaning and fix |
|---|---|
| `memory: N GiB available` (WARN) | Other workloads hold unified memory. The OAF services need about 8 GiB at most. Lower the models' `gpuMemoryUtilization` if you also deploy models here |
| `workloads: N existing inference container(s)` (WARN) | Other containers use the GPU. OAF does not touch them, but they share memory with OAF's models |
| `ports: N port(s) OAF uses are already taken` (WARN) | Something already listens on a port OAF needs (§10). A model port such as 8000 can be changed with `port:` on the deployment. The gateway port can be changed with `routing.gateway.port` |
| `nccl: no system libnccl` (WARN) | Matters only for models across a pair. Install `libnccl2` from the DGX OS repositories |
| `rdma-tools: ib_write_bw not installed` | `sudo apt install perftest`. The fabric check needs it |
| `containers cannot get the GPU` (FAIL) | The NVIDIA Container Toolkit CDI spec is missing. Run `sudo nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml`, then `docker run --rm --gpus all ubuntu nvidia-smi` |
| `docker engine not reachable` (FAIL) | `sudo systemctl start docker`. Run `oaf preflight` with sudo or as a member of the `docker` group |
| `clock not NTP-synchronised` (WARN) | `sudo timedatectl set-ntp true`. Skewed clocks break certificates and trace timelines |

### 9.2 The agent

| Symptom | Fix |
|---|---|
| `oaf-node` stays `activating`; journal shows `226/NAMESPACE` | The unit file predates the fix that lets systemd create `/var/lib/oaf`. Reinstall `deploy/systemd/oaf-node.service` from the current release and `systemctl daemon-reload` |
| `oaf status` shows `unreachable` | Is the agent running (`systemctl is-active oaf-node`)? Can the control node reach `<node address>:7701`? Is a firewall in the way? |
| A TLS error naming the wrong host, or `unknown authority` | The node has another node's certificate, or one from another CA. Copy that node's own files from `/etc/oaf/pki/nodes/<name>/` again and restart `oaf-node` |
| `open /etc/oaf/pki/oafd.crt: permission denied` | Run `oaf` commands with `sudo` on the control node |

### 9.3 Fabric

| Fabric check problem | Fix |
|---|---|
| `port N on <node> has no carrier` | Cable not seated, or connected to another port number than `cluster.yaml` says. Check with `ip -br link` on both ends |
| `<netdev> on <node> has MTU 1500, want 9000` | Add `mtu: 9000` to that interface in netplan and `netplan apply`. Both ends must match |
| `... has no IPv4 address in <subnet>; run fabric bring-up` | The rail has no address, or it is outside the subnet given in `links[].subnets` |
| `rail N: <a> and <b> are not in one subnet` | The two ends were addressed from different plans. Each rail needs both ends in the same `/30` |
| `no RDMA device or RoCE v2 GID` | `rdma link show` must list the device as ACTIVE. The kernel adds the RoCE v2 GID when the address is set. Re-run the check after a minute |
| `client on A: ... Couldn't connect` | The server side did not start in time, or a firewall blocks the exchange port (TCP 18515–18614 on the rail addresses) |
| Bandwidth below 85 Gb/s per rail | Check `ethtool <netdev>` for 200000Mb/s, check that MTU is 9000 on both ends, and check that nothing else is using the link. Each rail alone should measure about 110 Gb/s, and both together about 195 Gb/s |
| `these links form a 4-node ring` (validate) | Re-cable as two separate pairs, or add a QSFP switch (§3) |

### 9.4 Services

| Symptom | Cause and fix |
|---|---|
| `langfuse-web` restarting; logs show `FATAL ERROR: ... JavaScript heap out of memory` | An older OAF gave it too little memory. Current releases set 2 GiB and size the Node heap to fit. `oaf apply` with the current binary |
| A container restarts and `docker inspect` shows `OOMKilled=true` | It hit its memory limit (§10). Report it with `docker stats` output, because the limits come from measured usage |
| Langfuse login fails | The user is `admin@oaf.local`, and the password is in `/etc/oaf/secrets/langfuse-admin-password`. Sign-up is disabled on purpose |
| Gateway returns **401** | Missing or wrong `Authorization: Bearer` key. Use the value of the `secretRef` key file |
| Gateway returns **404 `model_not_found`** | The `model` name is not a deployment in `cluster.yaml`, or no models are deployed |
| Gateway returns **503 / upstream call failed** | The model's vLLM is not running or not healthy yet. Check `oaf status` and `docker compose -p oaf-model-<name> logs` |
| No traces in Langfuse | Check that `observability.langfuse.enabled` is true and that the gateway node can reach `<observability>:3000`. The gateway logs `BatchSpanProcessor.ExportError` when it cannot |
| `apply` says a licence must be accepted | Read the named page and add the `spec.licenseAcceptances` entry (§7) |
| `warning: cluster.yaml asks for bundle X; this oaf binary carries Y` | The binary is older or newer than the bundle the file was written for. It works, but upgrade the binary to match |

### 9.5 Models

| Symptom | Cause and fix |
|---|---|
| Model stays `starting` for many minutes | This is normal for large models (loading plus CUDA graph capture). The health check allows 30 minutes. `docker compose -p oaf-model-<name> logs -f` shows progress |
| vLLM exits with `No such file or directory` for the model path | The weights are not at `/var/lib/oaf/models/<catalog id>/` on that node (every node of a pair needs them) |
| vLLM exits with an out-of-memory or `not enough KV cache` error | Lower `maxModelLen` or raise `gpuMemoryUtilization`, keeping the node's total at 0.90 or less. Remember other workloads on the node |
| A pair model hangs at NCCL init | Run `oaf fabric check` first. Then look for `NCCL WARN` in the rank-0 logs. A wrong GID or a missing system libnccl are the usual causes |

### 9.6 Still stuck
Collect `oaf status`, `oaf preflight --json`, `oaf fabric check`, `journalctl -u oaf-node -n 200` and
the failing project's logs, and open an issue at <https://github.com/anindyar/openagentfabric/issues>.
**Remove secrets first:** never paste files from `/etc/oaf/secrets` or `.env` files.

## 10. Reference

### Ports

| Port | Service | Bound to |
|---|---|---|
| 4000 | agentgateway API (`routing.gateway.port`) | all interfaces |
| 3000 | Langfuse | all interfaces |
| 3001 | Grafana | all interfaces |
| 9090 | Prometheus | all interfaces |
| 3100 | Loki (only with `observability.logs.enabled`) | all interfaces |
| 9100 | node-exporter | all interfaces (host network) |
| 15000 | agentgateway admin UI | 127.0.0.1 only |
| 15020 / 15021 | agentgateway metrics / readiness | all interfaces |
| 7701 | oaf-node agent (mTLS) | all interfaces |
| 7702 | key service: apps rotate their gateway key (control node) | all interfaces |
| 8899 | router (Envoy in front of vSR) for `model: auto`, only with active routing rules (gateway node) | all interfaces |
| 8000 + n | model n's OpenAI API (rank 0 node), unless `port:` is set | host network |
| 29500 + n | model n's rendezvous between the nodes of a pair | host network |
| 18515–18614 | `ib_write_bw` exchange, only during a fabric check | rail addresses |
| 8443 | `oafd` management API (control node, HTTPS) | all interfaces |

### Memory limits per container

| Service | Limit | Idle usage measured on a DGX Spark |
|---|---|---|
| langfuse-web | 2 GiB | ~1.0 GiB |
| langfuse-worker | 1 GiB | ~0.55 GiB |
| clickhouse | 1.5 GiB | ~1.1 GiB |
| grafana | 1 GiB | ~0.37 GiB (peaks above 512 MiB when apps embed dashboards) |
| prometheus | 512 MiB | ~76 MiB |
| postgres | 512 MiB | ~140 MiB |
| loki (if `observability.logs.enabled`) | 384 MiB | ~63 MiB |
| seaweedfs | 384 MiB | ~180 MiB |
| valkey | 256 MiB | ~14 MiB |
| agentgateway | 192 MiB | ~17 MiB |
| vsr (router, keyword/context signals) | 128 MiB | ~36 MiB |
| vsr with reasoning signals (encoder) | 1792 MiB, counted in the gateway node's model budget | ~1.7 GiB |
| envoy (router) | 64 MiB | ~25 MiB |
| node-exporter | 96 MiB | ~51 MiB |

vLLM has no container limit: its share is set by `gpuMemoryUtilization`.

### Commands
`oaf validate`, `oaf preflight [--ssh]`, `oaf pki init`, `oaf node serve`, `oaf fabric check`,
`oaf install`, `oaf apply [--dry-run]`, `oaf status`, `oaf uninstall --yes [--purge]`,
`oaf keys issue [--admin]|rotate|list|serve`, `oaf models add`, `oaf serve`, `oaf secrets set`,
`oaf observability ...`, `oaf version`.
Every command has `--help`. Commands that act on the cluster take `-f` (cluster.yaml),
`--catalog`, `--pki-dir`, `--secrets-dir` and `--timeout`.
