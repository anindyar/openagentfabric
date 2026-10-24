# openagentfabric: plan

- **Status:** M0 approved 2026-10-17. M1 in progress
- **Date:** 2026-10-17
- **Related:** [architecture](architecture.md), [user guide](user-guide.md), [ADR-0001 stack](adr/0001-stack.md), [licence audit](licence-audit.md), [THIRD_PARTY.md](../THIRD_PARTY.md), [`schema/`](../schema)

## 1. Goal

One command turns 1–4 GB10 boxes (DGX Spark / Asus GX10) into a working, observable, routed LLM
platform, and one UI then runs it. Everything OAF deploys is an unmodified upstream image, pinned
by digest and driven from one declarative file.

**Non-goals for v1:** Kubernetes; switched fabrics and 4-way tensor parallelism (they need a QSFP switch);
multi-cluster; non-NVIDIA hardware; Langfuse EE features; hard-forking any upstream.

## 2. Architecture

```
                         ┌──────────────────────── control node ─────────────────────────┐
  browser ──HTTPS──────▶ │ oafd  (API + UI via go:embed + reconciler + renderers)        │
                         │   │  desired: /etc/oaf/{cluster.yaml,catalog/} (git)            │
                         │   │  secrets: /etc/oaf/secrets/ (0600, generated)               │
                         │   └──Connect/mTLS──┐                                            │
  clients ──OpenAI API──▶│ agentgateway ──▶ vSR (model: auto) ──┐                         │
                         │      │ explicit model ──────────────┐ │                         │
                         │ Langfuse(web,worker) PG CH Valkey S3* │ Prometheus  Grafana      │
                         └──────────────────────────────────────┼─┼─────────────────────────┘
                                         ┌──────────────────────┘ │
                     every node:  oaf-node (systemd, root) ──▶ docker compose projects
                                  vLLM (per deployment)  node_exporter  dcgm-exporter
                                  ◀════ QSFP rails (RoCE, MTU 9000) ════▶ peer nodes
  * S3 store = SeaweedFS if approved (licence audit F1)
```

### 2.1 Control loop
1. A user or the UI edits desired state through `oafd`. `oafd` checks it against the **schema**,
   then runs **semantic validation** (§4.3), then commits it to git. Invalid state is never committed.
2. The reconciler computes a **plan** (a diff of rendered artefacts per node and component) that
   can be shown with `oaf apply --dry-run`.
3. `oafd` pushes the rendered artefacts to each `oaf-node`. The agent writes them atomically and
   runs `docker compose up -d` for each project that changed, then reports health.
4. Steps are ordered by dependency: fabric → storage → weights → vLLM → gateway/vSR config →
   observability wiring. Every step is idempotent. Re-running with no changes does nothing.

### 2.2 Per-node layout (2-node pair, from `examples/cluster.pair.yaml`)

| Node | Roles | Compose projects |
|---|---|---|
| spark-1 | control, gateway, observability, inference | oaf (oafd), agentgateway, vsr, langfuse (+pg, ch, valkey, s3), prometheus, grafana, vllm-fast, vllm-think (rank 0), exporters |
| spark-2 | inference | vllm-think (rank 1), exporters |

## 3. Install flow (the shape of M1, which M5 completes)

`curl … | bash` → bootstrap verifies the signature → `oaf install` runs these steps:
1. **Preflight on every node:** OS/driver/CUDA versions, docker + compose + nvidia-ctk, arch,
   disk space, clock sync, serials, hostnames, and the management network.
2. **Cabling discovery:** find physical ports with carrier (`devlink port` maps netdevs to physical
   ports; each port is two netdevs, one per PCIe half). Match peers by LLDP or link-local probe,
   propose `fabric.links` (one entry per cable) and write them back to `cluster.yaml` once the user
   confirms. **Never reconfigure a port that already has addresses or traffic unless told to explicitly.**
   Reject topologies that are not allowed (a 4-node ring).
3. **Fabric bring-up:** set MTU 9000, assign IPs, detect the **RoCE GID index**, run `ib_write_bw`
   on each rail and on both rails together, and check the results against `fabric.acceptance`.
4. **Secrets and PKI:** cluster CA, node certificates, gateway keys, and DB passwords.
5. **Deploy the pinned components** from `bundle/versions.yaml`.
6. **Pull catalog models:** check disk, pull once, rsync over the fabric to each peer in the group,
   verify checksums.
7. **Acceptance test** (§6).

## 4. Declarative model

### 4.1 Files
- `cluster.yaml`: see `schema/cluster.schema.json`. The examples are in `examples/`.
- `catalog/models.yaml`: see `schema/catalog.schema.json`.
- `bundle/versions.yaml` (M1): component → image + digest + upstream licence hash. It ships with
  the bundle and the user never edits it.

### 4.2 Schema design choices (please review)
- `apiVersion: oaf/v1alpha1`, `kind`, `metadata`, `spec`. Kubernetes-like, so it is familiar and versionable.
- **Secrets only as `secretRef`.** The schema rejects inline key fields (`additionalProperties: false`).
- **`gpuMemoryUtilization` is required** on every deployment, with a maximum of 0.95.
- Served model names cannot be `auto`, because that name is reserved for vSR.
- Catalog sources must pin a **40-hex commit SHA**. Branch names are rejected.
- Catalog licence fields **`gated` and `redistributable` are required**. They drive the offline bundler.
- Catalog **serving profiles** are keyed by node count (`single`, `pair-tp2`, …), and each profile
  has its own **baseline**, because tok/s depends on the topology.
- Routing is an **OAF abstraction** (`defaults` + ordered `rules` with
  `reasoning`/`domain`/`keyword` signals), not raw vSR config. It is rendered into vSR's
  `config.yaml` (v0.3 format at vSR v0.4.0). This decouples us from vSR config churn. The cost is that
  advanced vSR features need explicit support in our renderer.
- `apps` / `agents` are reserved and loosely typed (experimental, M6).

### 4.3 Semantic validation (in `oafd`, beyond JSON Schema)
Each rule gets a unit test.
1. Node names are unique. **Exactly one** `control` node, at least one `gateway` node, and exactly one
   `observability` node in v1.
2. `fabric.topology` matches the node count: single=1, pair=2, triangle=3, pairs=4. The links must form exactly
   that graph: a pair has 2 parallel links; a triangle has 3 links forming K3; `pairs` has two disjoint
   pairs. **A 4-node cycle is rejected with an explanation.**
3. Each link endpoint is a known node, and no interface is used twice.
4. Every group is fully connected by direct links. TP/PP never spans the two halves of `pairs`.
5. Deployments: `catalogRef` exists; `placement` targets a node or group with the `inference` role; the profile's
   `nodes` equals the placement size; and the entitlement is present when the catalog entry requires one.
6. **Memory budget per node:** the sum of `gpuMemoryUtilization` over the deployments on a node is ≤ 0.90.
7. **Disk:** catalog `sizeGiB` for each placed model + `minFreeGiB` ≤ free space. Checked live at apply time.
8. Routing: `defaults.fast`/`fallback` and every rule `target` name a deployment that is **deployed and
   enabled**. A rule that `requires` an entitlement without a valid one is **dropped with a warning**,
   not treated as an error, so medical prompts fall through to the default.
9. Port collisions on each node.
10. `fabric.nccl.transport: auto` resolves to `socket` for a triangle and `ib` otherwise. Explicit `ib` on a
    triangle is a warning.
11. **Licence acceptance** (licence-audit §3.7): a catalog entry whose licence is gated, has no SPDX id, or
    is off the allowlist must have `license.acceptUrl`. Deploying it requires a matching
    `spec.licenseAcceptances` entry (`model/<id>`).
12. Catalog: unique ids and profile names; `tensorParallel × pipelineParallel == nodes` (one GPU per GB10);
    `source.entitled` ⇔ `entitlement` set.

## 5. Routing behaviour (rendered into agentgateway + vSR)
- A request with an explicit `model` that matches a deployment goes from agentgateway straight to that vLLM. vSR is bypassed.
- `model: auto` goes from agentgateway to vSR. vSR evaluates the rules in order: think → large, medical → DDx if
  entitled. If nothing matches, the request goes to `defaults.fast`.
- **Observe mode:** vSR classifies the request and records its decision as headers and a Langfuse trace attribute,
  but the request is served by `defaults.fast`. The UI shows "would have routed to X".
- An unhealthy backend means fallback to `defaults.fallback`, using agentgateway health checks and
  vSR reliability settings. Absent backends are never rendered.
- An M3 spike decides the exact integration: whether vSR runs standalone behind agentgateway as an
  OpenAI-compatible upstream, or as ext_proc.

## 6. Acceptance test (`oaf acceptance`, run after every install and apply)
1. Per deployment: `/health` is OK, and a fixed benchmark runs with the catalog `baseline.workload`. Pass if
   tok/s ≥ baseline × tolerance. With no baseline the result is "unmeasured" (strict mode fails).
2. Fabric: `ib_write_bw` per rail and dual rail against `fabric.acceptance`.
3. Routing: 3 prompts (plain → fast, reasoning → think, medical → ddx or fast when not entitled)
   land on the expected backend, checked through response headers and the Langfuse trace.
4. Langfuse: the traces for those prompts exist through the public API.
5. Output is a JSON report stored under `/var/lib/oaf/acceptance/<ts>.json` and shown in the UI.

## 7. Milestones and PR breakdown

Each PR is small and has tests. **Exit criteria** are what I will demonstrate at each review.

### M0: foundations (this delivery)
- [x] Licence audit + `THIRD_PARTY.md`
- [x] `docs/PLAN.md`, ADR-0001
- [x] `cluster.yaml` + catalog JSON Schemas, with examples
- **Exit:** owner approval of the stack, the schemas, and the open decisions in §9. ✅ 2026-10-17

### M1: `oaf install` skeleton (one pair, CLI only)
1. Repo scaffold: Go module, `make`, CI (lint, test, arm64 build, licence allowlist, THIRD_PARTY guard).
2. Schema + semantic validation package. Tests use the `examples/` files and negative cases.
3. `bundle/versions.yaml` format, plus pinning digests for agentgateway, vSR, vLLM and the observability stack.
4. `oaf-node`: systemd unit, mTLS Connect server, `compose up/down/status` for a project. ✅ `oaf node serve`, `oaf pki init`. The agent only touches `oaf-*` compose projects under `/var/lib/oaf/projects`. Applies are atomic and idempotent.
5. Preflight checks (each check is a unit with fake command runners in tests). ✅ `oaf preflight [--ssh user@host]`. It is read-only by construction (an allowlisted runner), and its tests replay a real capture from a busy DGX Spark. Landed before item 4 because it needs no agent.
6. Fabric: cabling discovery, MTU and IPs, GID detection, `ib_write_bw` runner. ◐ `oaf fabric check [--ssh node=user@host]`
   verifies each link (carrier, MTU, a shared subnet per rail, the RoCE v2 GID of each rail address) and runs `ib_write_bw`
   through the agents (`RailBandwidth`: B serves, A connects, each rail alone and then both together) against
   `fabric.acceptance`. `install` runs it after preflight. Rail resolution is shared with the vLLM renderer. Measured on
   two DGX Sparks (port 1, a free link): 111.6 Gb/s per rail and 196.0 Gb/s together. Remaining: assigning MTU and IPs on
   free ports (persisted through the OS network config) and proposing `fabric.links` from cabling discovery.
7. Renderers + golden tests: compose for each component. ✅ `internal/render`: per-node projects `node-exporter`, `gateway`
   (agentgateway), `langfuse` (web, worker, Postgres, ClickHouse, Valkey, SeaweedFS `weed mini`), `metrics` (Prometheus,
   Grafana) and `model-<name>` (vLLM; pairs run TP across the link with `--nnodes`/`--headless`, NCCL rails and GID from
   discovery). Secrets come from `/etc/oaf/secrets` (generated, 0600) and reach containers only through each project's
   0600 `.env`; configs hold none (gateway keys as SHA-256 hashes). GPUs via `gpus: all` (CDI). CI renders the example
   pair, runs `docker compose config` on every project and brings up the control-node projects for real.
   Known gaps: per-key rate limits are not
   rendered yet; Langfuse browser media uploads need the S3 port published (not yet); deployments over 3 nodes
   (triangle) are refused until the triangle work.
8. `oaf install` / `oaf apply --dry-run` / `oaf status` / `oaf uninstall`. ✅ `internal/reconcile` plans through the agents
   (`ApplyProject` dry run + `ListProjects`; a dropped model is removed), applies in dependency order, and reports status.
   `--dry-run` generates no secrets. `install` = preflight on every agent, then apply. Agents are placed by hand for now
   ([user guide](user-guide.md)); agent bootstrap over SSH and fabric address assignment (item 6) are the remaining M1 work.
   **On hardware (2026-10-18, one DGX Spark that also serves two production vLLMs):** the agent, then `oaf install` of the
   control services by hand. Everything was healthy. Gateway auth worked (401 without a key), Langfuse init keys worked,
   every Prometheus target was up, and a re-run was a no-op. `uninstall` left no containers or ports (volumes kept), and
   a reinstall took 18 s and reused the existing secrets and data. The vLLMs were not touched. The run found two bugs, both
   fixed: the systemd unit failed when `/var/lib/oaf` did not exist, and `langfuse-web` ran out of Node heap under its
   memory limit. Every stack container is now capped (about 8 GiB in total) so that it cannot push the OOM killer toward
   a vLLM.
- **Exit:** on the real pair, a clean install brings up every pinned component healthy; a re-run is a no-op;
  uninstall leaves the hosts clean; rails measure ≥ 85 Gbps each.

### M2: model pipeline
HF search (via the HF API), disk check, pull, rsync fan-out over the fabric, checksum, vLLM render
(LD_PRELOAD, NCCL env, TP/PP across the group), health, benchmark, and writing measured baselines back to the catalog.
- **Exit:** both example models deploy (gpt-oss-120b TP=2 across the pair), and baselines are recorded.

### M3: agentgateway + vSR
Render the agentgateway config, keys and routes; render the vSR config from deployed models only;
observe/active mode; fallback; a policy editor in the UI (first UI PRs); the 3-prompt acceptance test.
Decide whether an agentgateway fork is needed at all (the goal is that it is not).
- **Exit:** the routing acceptance test passes in both modes; killing a backend falls back to the default.
- **Status (2026-10-24):** vSR rendered (ADR-0003): a `router` project (vSR + Envoy) when a rule is
  active, generated from enabled deployments only; keyword, context and reasoning signals; observe
  and active; fallback on a failed health check and on vSR down; decisions traced as `oaf.route`.
  Proven by hand on a DGX Spark with two real models (qwen-flash, GLM-5.3-Flash). No agentgateway
  fork needed. Open: the automated 3-prompt acceptance test, the UI policy editor, fetching the
  encoder artifact (M2 pipeline), domain signals (classifier not pinned).

### M4: observability in one UI
Langfuse wiring (OTEL from agentgateway/vSR), Grafana provisioning (datasources, OAF dashboards),
Grafana embedded in the UI, Langfuse views through its API, cost and latency pages.
- **Exit:** a request is visible end to end, from the trace to the GPU metrics, without leaving the OAF UI.
  Langfuse on SeaweedFS has been verified.

### M5: signed bundle and full-stack install
Signed online and offline bundles, `install.sh`, upgrade (bump digests → reconcile, with rollback),
uninstall, licences shipped in the bundle. **Gated on the LEGAL items in the licence audit.**
- **Exit:** an air-gapped install on wiped nodes succeeds, and an upgrade from N-1 passes acceptance.

### M6: apps and agents
App profiles (HMS, vibecomputing), local buzz relay, memory system. These will be scoped in their own ADRs.

## 8. Risks

| Risk | Mitigation |
|---|---|
| GB10 quirks (GID index, libnccl, memory grab) change with DGX OS updates | Detect on every bring-up and never hard-code. Preflight pins known-good DGX OS and driver versions and warns otherwise |
| vSR config format churn (v0.3 config at release v0.4.0) | Use an OAF routing abstraction, version-specific renderers, and golden tests per pinned vSR version |
| Langfuse on a "community-supported" S3 store | The acceptance test covers traces. RustFS is the fallback |
| dcgm-exporter may not support GB10 unified memory | M1 spike. Fallback is a small exporter reading `nvidia-smi` |
| A single control node is a single point of failure | The data plane keeps working without `oafd`. A git mirror on peers allows `oaf promote` |
| Licence changes upstream (Langfuse is now owned by ClickHouse, Inc.) | CI licence-hash check at every pin bump |

## 9. Decisions (resolved 2026-10-17 unless marked)
1. ✅ Stack in ADR-0001 accepted.
2. ✅ Valkey replaces Redis; SeaweedFS replaces MinIO. CLAUDE.md is updated.
3. ✅ Schemas accepted. Licence acceptance was added (`license.acceptUrl`, `spec.licenseAcceptances`).
4. ✅ Domain `openagentfabric.dev` (Cloudflare). It is used for the schema `$id`, the install URL, and the Go
   module path `openagentfabric.dev/oaf` (a vanity import, served from Cloudflare when the repo goes public).
5. ✅ Licences: user-acceptance flow instead of OAF redistributing (licence-audit §3.7).
6. ⏳ DDx signing-key holder and weight delivery. Not needed until M5.
