# openagentfabric architecture

- **Audience:** contributors and operators who want to know how OAF works and why it is built this way.
- **Related:** [user guide](user-guide.md), [PLAN.md](PLAN.md) (milestones, schemas, validation rules),
  [ADR-0001](adr/0001-stack.md) (language and protocol choices), [licence audit](licence-audit.md).
- **Status markers:** ✅ built and tested on hardware · ◐ partly built · ⏳ planned (milestone).

## 1. What OAF is

OAF turns a few GB10 machines (NVIDIA DGX Spark or Asus GX10: arm64, 128 GB unified memory and a
dual-port 200 GbE ConnectX-7) into one LLM platform with a single endpoint, routing, traces and
metrics. It installs and runs that platform, but it does not reimplement any part of it: the
serving, routing and observability are well-known open-source projects that OAF pins, configures,
connects and keeps healthy.

## 2. Principles

These come from [CLAUDE.md](../CLAUDE.md), and every design choice below follows from them.

1. **Orchestrate upstreams, never fork them.** agentgateway, vLLM, vSR, Langfuse, Prometheus and
   Grafana run as unmodified images, pinned by digest. OAF talks to them only through their config
   files and HTTP APIs. An upgrade is a new pin in `bundle/versions.yaml` followed by `oaf apply`.
2. **One declarative source of truth.** `cluster.yaml` and `catalog/models.yaml` describe everything.
   Every other artefact (compose files, gateway config, Prometheus config, `.env` files) is rendered
   from them. Nothing exists only in a UI.
3. **Idempotent and safe to re-run.** Every operation converges to the desired state, and re-running it
   with no changes does nothing.
4. **Safe next to other workloads.** Inspection is read-only by construction. The agent touches only
   its own `oaf-*` compose projects. Fabric ports that already have addresses are never reconfigured.
   OAF's containers are memory-capped so they cannot push the kernel's OOM killer toward someone else's model.
5. **Hardware facts are detected, not hard-coded.** The RoCE GID index, rail addresses and link state
   are read on every run, because they change with the DGX OS version and the number of addresses.

## 3. System overview

```
                    management network: control RPC, client API, web UIs
        ┌──────────────────────────────┴───────────────────────────────┐
        │                                                              │
┌───────┴─────────────────────────────────┐   ┌────────────────────────┴───────────┐
│ node A: control, gateway,               │   │ node B: inference                  │
│         observability, inference        │   │                                    │
│                                         │   │                                    │
│ oaf CLI ✅  (oafd ⏳: API, UI, reconciler)│   │                                    │
│   /etc/oaf: cluster.yaml, catalog,      │   │                                    │
│             pki, secrets                │   │                                    │
│   │  Connect RPC over mTLS (:7701)      │   │                                    │
│   ├─────────────▶ oaf-node ✅           │   │ oaf-node ✅  ◀── from node A        │
│   │                 │ docker compose    │   │   │ docker compose                 │
│   │                 ├ oaf-gateway     ✅ │   │   ├ oaf-model-think (rank 1)    ✅  │
│   │                 ├ oaf-vsr         ⏳ │   │   └ oaf-node-exporter          ✅  │
│   │                 ├ oaf-model-*     ✅ │   │                                    │
│   │                 ├ oaf-langfuse    ✅ │   │                                    │
│   │                 ├ oaf-metrics     ✅ │   │                                    │
│   │                 └ oaf-node-exporter ✅│   │                                    │
└───┼─────────────────────────────────────┘   └────────────────────────────────────┘
    │            vLLM rank 0 ◀═══ QSFP cable: 2 rails, RoCE v2, MTU 9000 ═══▶ vLLM rank 1
    └─────────────────────────────────────────────▶ (RPC to node B's agent)

clients ──OpenAI API, Bearer key──▶ agentgateway :4000 ──▶ vLLM ── traces ──▶ Langfuse
```

The compose projects:
- `oaf-gateway` runs agentgateway.
- `oaf-model-<name>` runs one vLLM per deployment, on every node of its placement.
- `oaf-langfuse` runs Langfuse web and worker, with Postgres, ClickHouse, Valkey and SeaweedFS.
- `oaf-metrics` runs Prometheus and Grafana.
- `oaf-node-exporter` runs on every node.
- `oaf-vsr` arrives in M3.

### 3.1 The `oaf` binary
One static Go binary, with no runtime dependencies on the host ([ADR-0001](adr/0001-stack.md)):

| Subcommand | Role | Status |
|---|---|---|
| `oaf validate`, `preflight`, `pki`, `fabric check`, `install`, `apply`, `status`, `uninstall` | CLI on the control node | ✅ |
| `oaf node serve` | the node agent, `oaf-node.service` on every node | ✅ |
| `oafd` | long-running control plane: API, embedded web UI, continuous reconcile | ⏳ M3–M4 |

Until `oafd` exists, the CLI *is* the control plane. It reconciles once per command, using the same
packages that `oafd` will run in a loop.

### 3.2 Packages

| Package | Responsibility |
|---|---|
| `internal/config` | Loads `cluster.yaml` and the catalog, runs JSON Schema plus semantic validation (topology, ports, memory budget, licences, routing targets), and holds the port plan |
| `internal/preflight` | Read-only node inspection, run through an allowlisted command runner |
| `internal/fabric` | QSFP discovery (ports → two netdevs → RDMA device → GID table), rail resolution, `ib_write_bw` runner |
| `internal/render` | Pure functions from desired state to per-node compose projects and upstream configs. Golden-file tested |
| `internal/secrets` | Generates secrets on first use under `/etc/oaf/secrets` (0600) and never overwrites one |
| `internal/pki` | Cluster CA, the `oafd` client certificate, node server certificates |
| `internal/node` | The agent: Connect RPC handlers, atomic project writes, `docker compose` driver |
| `internal/reconcile` | Plans and applies across agents, reports status, checks the fabric |

### 3.3 The node agent
`oaf-node` runs as root under systemd and listens on TCP 7701 with mTLS. It accepts only the cluster's
`oafd` certificate. Its API ([`node.proto`](../proto/oaf/node/v1/node.proto)):

| RPC | Effect |
|---|---|
| `Info`, `Preflight`, `Fabric`, `ListProjects`, `ProjectStatus` | read-only |
| `ApplyProject` (`dry_run`) | writes a project's files atomically (swap of a new directory), then `docker compose up -d`. With `dry_run` it only reports which paths would change |
| `RemoveProject` | `docker compose down`, optionally with volumes |
| `RailBandwidth` | one side of an `ib_write_bw` run, only on this node's QSFP RDMA devices, with a bounded port, duration and queue-pair count |

**Limits on what the agent can do.** Project names are DNS labels, and compose projects are always
`oaf-<name>` under `/var/lib/oaf/projects`, so the agent cannot address containers it did not create.
File paths in a project must be clean relative paths, and modes are limited to 0777. Inspection goes through
`hostexec.ReadOnly`, which refuses any command that is not on its read-only allowlist.

## 4. How a change flows

### 4.1 Desired state → running containers
```
cluster.yaml + catalog ──validate──▶ render (pure) ──▶ per-node projects ──plan──▶ apply
                                       ▲    ▲                                ▲
                       fabric discovery │    │ secrets (store, or preview)    │ agents: ApplyProject
                       from the agents  │    │                                │ in dependency order
```

1. **Validate.** JSON Schema first, then semantic rules ([PLAN §4.3](PLAN.md)). An invalid file is never applied.
2. **Discover.** For deployments that span nodes, the agents report their QSFP ports, so the renderer
   can choose the rail addresses, RDMA devices and GID index.
3. **Render.** This is deterministic: the same inputs give byte-identical files. Each node gets
   `node-exporter`, plus `gateway`, `langfuse` and `metrics` according to its roles, plus
   `model-<name>` for each deployment placed on it. Secrets go only into each project's `.env`
   (0600) and reach containers through compose interpolation. Config files never contain secrets:
   agentgateway expands `$VARS` itself, and client keys are stored only as SHA-256 hashes.
4. **Plan.** Each agent compares the rendered files with what it has (`ApplyProject` with `dry_run`)
   and lists its projects. A project that is no longer rendered (for example a disabled model) is
   removed. `oaf apply --dry-run` stops here and uses placeholder secrets, so it generates nothing.
5. **Apply.** Removals go first (they free memory and ports). Then, in dependency order:
   node-exporter, Langfuse, metrics, models, gateway. Every desired project is converged, even an
   unchanged one, because `compose up` also restarts stopped containers.

### 4.2 A request
```
client ──Bearer key──▶ agentgateway :4000 ──▶ vLLM (by model name) ──▶ response
                           │  model: auto → vSR ⏳ (until M3: routing.defaults.fast)
                           └─ OTLP span ──▶ Langfuse /api/public/otel/v1/traces (latency, tokens, model,
                                            user/session from X-OAF-User / X-OAF-Session)
Prometheus scrapes: agentgateway :15020, vLLM /metrics, node-exporter :9100 → Grafana
```

An explicit `model` name goes straight to that deployment. `model: auto` will go through vSR (M3),
which picks a model from the request's signals (reasoning → large model, medical → DDx when
entitled). In *observe mode*, vSR only records what it would have chosen.

### 4.3 Install
`oaf install` = preflight on every agent → fabric check on every link (configuration and
`ib_write_bw`) → apply. It stops at the first failed stage unless `--force` is given. Cabling discovery,
fabric address assignment, bootstrapping agents over SSH (all M1), weight download and fan-out (M2),
the acceptance test, and the signed one-line installer (M5) slot into this pipeline without changing its shape.

## 5. Topology: pairs, not a 4-node cluster

**Decision:** OAF builds clusters out of **pairs**. A model runs on one node, or with tensor
parallelism across the two nodes of a pair (TP=2). Four machines are two independent pairs, not one
4-way cluster. Three machines form a triangle, which serves pairs and singles.

There are two reasons, the first economic and the second physical.

### 5.1 The speed sweet spot is under ~20B active parameters
Generating each token streams every *active* weight from memory once. A GB10 has about
**273 GB/s** of memory bandwidth, so a hard upper bound for single-stream decode is:

> tokens/s ≤ 273 GB/s ÷ (active parameters × bytes per parameter)

| Model (active / total parameters) | Precision | Bytes read per token | Ceiling on one node | Fits on |
|---|---|---|---|---|
| Qwen3-30B-A3B (3.3B / 30.5B) | FP8 | ~3.3 GB | ~80 tok/s | 1 node |
| gpt-oss-120b (5.1B / 117B) | MXFP4 | ~2.7 GB | ~100 tok/s | 1 node |
| GLM-4.5-Air (12B / 106B) | FP4 | ~6 GB | ~45 tok/s | 1 node |
| any 20B-active model | FP4 | ~10 GB | ~27 tok/s | — |
| Qwen3-235B-A22B (22B / 235B) | FP4 | ~11 GB | ~25 tok/s (~2× on a pair) | 1 pair |
| Kimi K2 (32B / 1T) | FP4 | ~16 GB | ~17 tok/s | 5+ nodes |
| DeepSeek-V3/R1 (37B / 671B) | FP4 | ~19 GB | ~15 tok/s | 3+ nodes |

These are ceilings: real throughput is lower, because of KV-cache reads, attention and kernel
efficiency. OAF measures real numbers per catalog profile in M2. The shape is what matters.
Below about 20B active parameters, a GB10 decodes fast enough for interactive agents. Above it, a
single stream gets too slow for agent loops that make many sequential calls, and this is the owner's
experience on the hardware too.

Now compare that with **capacity**. A node can give about 115 GB (0.90 of 128 GB) to models, and a
pair about 230 GB. At FP4 that holds models up to roughly 400B total parameters with room left for KV cache.

**As of October 2026, no open model both needs more than two nodes and has fewer than ~20B active
parameters.** The models that only fit on three or more nodes (DeepSeek-V3/R1, Kimi K2, and others
of their size) have 32–37B active parameters, and they would run below the speed sweet spot even if
the fabric scaled perfectly. Every model in the sweet spot fits on one node or one pair. A 4-node
cluster would therefore buy capacity for models that are too slow to be worth serving here. Two pairs
serve twice as many sweet-spot models, or the same models with redundancy.

### 5.2 The switchless fabric cannot do 4-way well anyway
- **No switch.** Each GB10 has two QSFP ports, so without a switch four nodes can be wired either as
  a ring or as two pairs. **A ring breaks NCCL.** Its tree and ring channels need traffic between
  nodes the ring makes non-adjacent, and RDMA cannot be forwarded through a node. `oaf validate` rejects
  rings for this reason. A true 4-way cluster needs a QSFP switch, which costs more than another
  machine and adds a failure point.
- **One cable is enough for a pair.** One cable gives two rails (one per PCIe half of the port), and
  both can be used at once. Measured between two DGX Sparks: **111.6 Gb/s per rail and 196 Gb/s on
  both rails together**. Tensor parallelism across two nodes needs one all-reduce per layer. That works
  over one direct link, but every additional hop or peer adds latency to every token.
- **A triangle works only over sockets.** On a 3-node point-to-point mesh, NCCL over RDMA is
  unreliable on GB10, so OAF uses `NCCL_IB_DISABLE=1` (sockets) there. Pairs keep the fast RoCE path.

### 5.3 What OAF does with this
- **Topologies:** `single` (1), `pair` (2), `triangle` (3), `pairs` (4 = two disjoint pairs). The validator
  enforces them. Multi-node deployments currently support groups of exactly two nodes.
- **Catalog profiles** are keyed by node count (`single`, `pair-tp2`), and each profile has its own
  baseline, because tok/s depends on the topology.
- **Placement:** small, fast models go one per node. A larger model that is still in the sweet spot
  (gpt-oss-120b, or a 22B-active MoE at FP4) runs either on one node or across a pair for about
  twice the decode ceiling.

### 5.4 When to revisit
Reopen this decision if any of these happen:
1. A model appears that both needs more than ~230 GB of weights and has fewer than ~20B active parameters.
2. A QSFP switch becomes part of a supported deployment, which removes the ring problem.
3. Expert parallelism over the fabric becomes efficient enough that the bandwidth arithmetic in §5.1 changes.

## 6. Hardware details OAF relies on

| Fact | Consequence in OAF |
|---|---|
| Each QSFP port appears as **two netdevs**, one per PCIe domain (port 0 = `enp1s0f0np0` + `enP2p1s0f0np0`) | A cable is modelled as two rails, each with its own subnet. NCCL gets both RDMA devices |
| The **RoCE v2 GID index moves** (seen at 3 and at 5) as addresses are added | Detected on every run: OAF chooses the RoCE v2 GID whose address is the rail IP. Both rails must agree, because NCCL takes one index |
| Containers get the GPU through **CDI**, and no `nvidia` runtime is registered (docker 29 on DGX OS) | Compose files use `gpus: all` and never `runtime: nvidia` |
| **Unified memory:** `nvidia-smi` shows no memory usage, and vLLM takes ~90% by default | Memory is read from `/proc/meminfo`. Every deployment states `gpuMemoryUtilization`, and the total per node is ≤ 0.90. Stack containers are capped (§7) |
| vLLM on GB10 may need the system **libnccl via `LD_PRELOAD`** | Preflight checks for libnccl. The image choice is settled in M2 |
| Weights must be on **every node of a TP group** | Download once, rsync over the fabric (M2), and mount read-only with `HF_HUB_OFFLINE=1` |

## 7. Security and isolation

- **Transport.** All control traffic is Connect RPC over mTLS with a per-cluster CA. The agent checks
  that the client is `oafd`, and the client checks the node's name. The CA key stays on the control node.
- **Secrets** are generated per install (32 random bytes, hex), stored one per file at 0600, never
  overwritten (except a gateway key being rotated, which is replaced atomically), never in
  `cluster.yaml`, never in git, and passed to containers only through 0600 `.env` files. The
  repository and fixtures contain none.
- **App keys.** Each app gets its own `routing.keys` entry (`oaf keys issue`). The key service on :7702
  rotates a key for whoever presents it, and nothing else: the presented key is the credential, compared
  by digest in constant time, failures are slowed, and a key rotates at most once per 30 seconds.
  It is plain HTTP like the gateway, so both belong on a trusted network.
- **Least exposure.** The agentgateway admin UI is bound to 127.0.0.1. The gateway requires a Bearer key
  (strict mode). Langfuse sign-up is disabled, and Grafana and Langfuse have generated admin passwords.
  Langfuse and Grafana telemetry, update checks and plugin downloads are off, so the stack works air-gapped.
- **Co-tenancy.** OAF often shares machines with existing workloads. Preflight reports them and OAF never
  touches them. Stack containers have memory limits (about 7 GiB in total, sized from measured usage),
  so an OAF container that misbehaves is killed and restarted on its own instead of starving a model.
- **Licences.** Upstream images and models whose terms need acceptance (for example NVIDIA's CUDA EULA
  in the vLLM image) are refused until a `spec.licenseAcceptances` entry records who accepted which
  page, and when. Grafana (AGPL) is embedded only through iframes and its HTTP API. Langfuse `ee/` code
  is never used ([licence audit](licence-audit.md)).

## 8. Failure behaviour

| Failure | Effect |
|---|---|
| Control node or `oafd` down | The data plane keeps serving: the gateway and vLLM are configured statically. No changes can be applied until it is back. A peer can be promoted from a git mirror of `/etc/oaf` ⏳ |
| Agent down | That node keeps running its containers. `oaf status` shows it as `unreachable`, and apply fails for that node only |
| A container crashes | Docker restarts it (`unless-stopped`). `oaf apply` converges anything that stopped |
| A stack container exceeds its memory cap | The cgroup kills only that container, and it restarts |
| Model backend unhealthy | The gateway returns 503 for that model. Fallback to `routing.defaults.fallback` arrives with vSR ⏳ |
| Fabric link degraded | `oaf fabric check` fails on it, and install stops before deploying |

## 9. Testing strategy

- **Unit tests** for every package. Inspection and fabric code is tested by **replaying recorded command
  output from a real DGX Spark** (scrubbed of MAC addresses, GPU UUIDs and home paths).
- **Golden files** for every renderer, so any change to a generated file shows up in review.
- **Integration (CI):** `docker compose config` on every rendered project, then the control-node stack is
  brought up for real and probed (Langfuse health and init keys, Prometheus, Grafana, node-exporter,
  gateway auth).
- **Hardware runs** on DGX Sparks: fabric check and bandwidth, and install → re-run (no-op) → uninstall →
  reinstall on a node that also serves production models. Recorded in [PLAN.md](PLAN.md).
- **Acceptance test** ⏳ after every install: model health and tok/s against the catalog baselines, rail
  bandwidth, routing prompts landing on the expected model, and the matching Langfuse traces.
