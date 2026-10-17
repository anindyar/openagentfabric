# CLAUDE.md — openagentfabric

## What this is
openagentfabric (OAF) is an open-source (Apache-2.0) control plane and UI that gives a single place to run an agent/LLM platform on a small GPU cluster, starting with 2–4 node NVIDIA DGX Spark / Asus GX10 (GB10, arm64, 128 GB unified memory, dual-port ConnectX-7 200GbE).

It covers:
- Cluster: nodes, QSFP pairs, link health, GPU and disk.
- Models: search Hugging Face, choose a pair or node, download, fan out to peers, deploy on vLLM, check health, benchmark.
- Routing: agentgateway backends, routes and keys; the vSR (vLLM Semantic Router) policy editor; observe mode vs active mode.
- Observability: Langfuse traces, cost and latency; Grafana/Prometheus metrics, shown inside the OAF UI.
- Install: **installing OAF installs the whole stack.** One command (`curl -fsSL <url>/install.sh | bash`, or `oaf install --offline <bundle>`) does preflight, cabling/pair setup, and then deploys agentgateway, vSR, vLLM, Langfuse (+ its Postgres/ClickHouse/Valkey/SeaweedFS), Prometheus, Grafana and the OAF UI itself on every node. It then pulls the catalog models and runs the acceptance test. The user installs nothing else by hand.

## Architecture rules (do not break)
1. **Orchestrate upstreams, do not hard-fork them.** agentgateway, vSR, vLLM, Langfuse and Grafana run as unmodified, pinned upstream images. OAF talks to them through their config files, admin APIs and HTTP APIs.
2. The agentgateway fork exists **only** for UI/admin-API extensions that upstream does not accept. Keep that diff minimal, rebase it on every upstream release, and send generic changes upstream first.
3. Licences: agentgateway, vLLM and vSR are Apache-2.0. Langfuse core is MIT; never copy anything from its `ee/` directories. Grafana is AGPLv3, so embed it through iframes or its HTTP API and never vendor its code. Record the licence of every dependency in `THIRD_PARTY.md`.
4. One declarative source of truth: `cluster.yaml` (topology, pairs, models, routing, apps, agents) plus `catalog/models.yaml`. The UI edits these files and a reconciler applies them. Never create state that exists only in the UI.
5. The runtime is docker compose + systemd on each node, driven by a small agent on each node. Kubernetes is out of scope for v1.
6. Everything must be idempotent and safe to re-run. Pin every image by digest.
7. **OAF owns the lifecycle of every component:** install, configure, upgrade (bump the pinned digest in the bundle, then reconcile), health, and uninstall. Upstreams stay unmodified, but OAF is the only thing a user installs or upgrades. Every component version is pinned in a single `bundle/versions.yaml`, and each OAF release is tested as one set.

## Hard-won hardware facts (GB10 / CX-7)
- No switch: 2 nodes = 1 dual-rail pair, 3 nodes = triangle, 4 nodes = two separate pairs. A 4-node ring breaks NCCL, because tree channels need links between non-adjacent nodes and RDMA cannot be forwarded. A true 4-way cluster needs a QSFP switch.
- Each GB10 has **2 physical QSFP ports**. The OS shows each one as **two netdevs, one per PCIe domain** (port 0 = `enp1s0f0np0` + `enP2p1s0f0np0`, port 1 = `enp1s0f1np1` + `enP2p1s0f1np1`). Those two halves are the "dual rail": **one cable = 2 rails** (each needs its own IP/subnet). Verified with `devlink port` on DGX OS 2025-10 (kernel 6.17).
- Detect the RoCE GID index on every bring-up; it has changed from 3 to 4. It depends on how many addresses the netdev has. On one netdev, RoCE v2 GIDs were seen at index 3 (first IP) and 5 (second IP). Choose the RoCE v2 GID whose address is the rail IP. Use MTU 9000 and check it with `ib_write_bw` on each rail (about 92 Gbps per rail, about 185 Gbps with both).
- vLLM on GB10 needs `LD_PRELOAD` of the system libnccl (2.30.x). On a 3-node point-to-point mesh, use `NCCL_IB_DISABLE=1` (sockets).
- Each vLLM grabs about 90% of unified memory by default, so always set `--gpu-memory-utilization` per model.
- Weights must be present on every node in a TP/PP group. Pull them once and rsync them over the fabric. Check disk space before every pull.

## Routing defaults (vSR)
- Generate the vSR config from the models that are actually installed. Never route to a backend that is absent.
- Clients that name an explicit model bypass vSR.
- `model: auto` → default fast model. Think signals → large model. Medical signals → DDx (only if an entitlement exists).
- If a backend is unhealthy, fall back to the default model.

## Licensing / entitlements
Gated models such as DDx use an offline ed25519-signed entitlement file, bound to the customer, node serials and an expiry date. It must work air-gapped.

## Repo layout (proposed)
- `cmd/oaf` — the single binary (ADR-0001): CLI, `oafd` control plane (API + reconciler) and `oaf-node` agent as subcommands
- `internal/` — Go packages
- `ui/` — web UI
- `bundle/` — installer, compose templates, catalog
- `deploy/compose/` — compose files
- `docs/`
- `test/e2e/`

Language: use whatever matches the agentgateway fork for anything inside it. For OAF itself, propose a choice with reasons in `docs/adr/0001-stack.md` before writing code.

## Working rules
- Begin with `docs/PLAN.md` and ADRs. Get owner approval before writing large amounts of code.
- Work in small PRs, each with tests. The e2e acceptance test runs after every install: per-model health and tok/s vs the catalog baseline, rail bandwidth, 3 routing prompts landing on the expected models, Langfuse traces present.
- Never commit secrets. Generate secrets on each install.
- Build arm64 first, then multi-arch.

## Milestones
- M0: ADRs, `cluster.yaml` and catalog schemas, upstream licence audit.
- M1: `oaf install` skeleton (preflight → pair/fabric bring-up → deploy pinned components with compose) for one pair, CLI only.
- M2: model pipeline (HF search → download → fan-out → vLLM deploy → benchmark).
- M3: agentgateway + vSR rendering, and a policy editor with observe mode.
- M4: Langfuse + Grafana embedded in one UI.
- M5: signed bundle and one-curl install of the **full stack** (every component above plus OAF), offline mode, upgrade/uninstall.
- M6: app profiles (HMS, vibecomputing) and agents (local buzz relay + memory system).
