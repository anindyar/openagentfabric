# ADR-0002: Management API, gateway features, cluster growth and 1:1 HA

- **Status:** Accepted (owner, 2026-10-24); HA (§6) to be reviewed before its build starts
- **Date:** 2026-10-24
- **Deciders:** owner

## Context

OAF is driven from the CLI. An app console (Garrison, the first app) needs to manage the whole
platform for an admin: models (local and external), routes, guardrails, keys, nodes, and growing
the cluster with more boxes. The owner asked for:

1. The Agent Gateway's capabilities in the UI: add external models, guardrails, routes.
2. A guided flow that onboards more DGX boxes with a generated script, recommending a topology:
   - a) +1 node over ConnectX-7: this box and the new one become a pair (TP=2 models, e.g.
     GLM-5.3-Flash NVFP4, ~190 GB).
   - b) +1 node over LAN, **fully redundant 1:1** (HA): a second appliance that takes over when the
     first fails. User failover is over LAN, most likely through an external load balancer.
   - c) +1 pair: two new boxes cabled to each other over ConnectX-7 and to this box over LAN. A small
     model stays here, GLM runs on the pair, and `model: auto` routes complex queries to the pair.
3. External models must warn loudly without guardrails and recommend basic ones.

Constraints that do not change (CLAUDE.md): `cluster.yaml` is the only source of truth; upstreams
stay unmodified and pinned; everything idempotent; works air-gapped.

## Decision

### 1. `oafd`: a management API over `cluster.yaml`

`oaf serve` (the `oafd` role of the single binary) runs on the control node: an HTTPS API that reads
and edits `cluster.yaml` through the same YAML-tree edits the CLI uses, validates the result, shows a
diff, and applies through the existing reconciler. It never holds state of its own.

- **Auth:** a management token per app, `oaf keys issue <app> --admin` → `OAF_API_URL`,
  `OAF_API_TOKEN` (secret `oaf-admin-<app>`, rotatable). Apps proxy it for their admins only; the
  token never reaches a browser.
- **Writes are two-step:** `POST …/plan` returns `{diff, warnings, errors, plan_id}`; `POST
  /api/v1/apply {plan_id}` applies it (409 if `cluster.yaml` changed since). Garrison maps that onto
  its approvals (propose → approve → apply → verify).
- **Not an embedded agentgateway UI.** agentgateway's admin UI edits its own live config, which the
  reconciler overwrites; it also binds to 127.0.0.1 on purpose. Native pages over this API keep one
  source of truth. (A read-only view of the rendered gateway config is exposed instead.)

API (JSON; `/api/v1`):

| Method | Path | Purpose |
|---|---|---|
| GET | `/status` | nodes, deployments, components, health (what `oaf status` prints) |
| GET | `/config` | `cluster.yaml` (secrets are refs only) + rendered gateway config (read-only) |
| GET | `/catalog` | catalog models with profiles and memory needs |
| GET/POST/DELETE | `/models` | local deployments (`oaf models add` as an API); POST/DELETE return a plan |
| GET/POST/PUT/DELETE | `/providers` | external providers (see §2); plans |
| GET/POST/PUT/DELETE | `/guardrails` | guardrail policies (see §3); plans |
| GET/PUT | `/routing` | defaults, rules, mode `observe`/`active` (see §4); plans |
| GET/POST/DELETE | `/keys`, POST `/keys/{name}/rotate` | app keys (existing key service) |
| POST | `/plans/{id}/apply`, GET `/plans/{id}` | apply a plan; follow it |
| POST | `/nodes/join-tokens` | one-time join token + command (see §5) |
| GET | `/nodes` | joined and pending nodes, hardware, preflight, links |
| POST | `/topology/recommend` | options a/b/c for the pending nodes, with what each enables |
| POST | `/topology/plan` | the chosen option as a plan (nodes, fabric, groups, models, routing) |
| GET | `/events` | SSE: plan progress, join progress, link tests |

### 2. External providers

`spec.routing.providers[]`: `{name, type: openai|anthropic|gemini|azure-openai|openai-compatible,
endpoint?, models: [{name, upstream}], secretRef, guardrails: [policy…], allowEgress: true}`.
Rendered as agentgateway AI backends; the key lives in OAF secrets, never in `cluster.yaml`.

- Off by default; adding one is an explicit `allowEgress: true` per provider.
- A provider (or route to it) with no guardrail policy is **valid but warned** on every plan:
  "External model without guardrails: prompts, code and data leave the appliance unfiltered. This is
  extremely dangerous; use with care." The UI shows it in red and offers the **basic** policy (§3).
- External models are reachable by explicit model name only, and by `auto` only when a routing rule
  names them.

### 3. Guardrails

`spec.routing.guardrails[]` (named policies), attached to providers, models or keys. Rendered to
agentgateway prompt guards (unmodified upstream features):

- **regex**: built-in detectors (email, phone, SSN, credit card where the pinned version has them)
  and custom patterns; action `reject` (request) or `mask` (response).
- **moderation / webhook** where agentgateway supports it (OpenAI moderation, custom webhook).
- **limits**: per-key model allow-lists and requests per minute (existing `APIKey` fields).

`basic` (the recommended starting point): reject prompts containing credentials and private keys
(API keys, AWS keys, PEM blocks, passwords in URLs), mask emails/phones/card numbers in responses,
and a per-key rate limit. The exact detectors are pinned to what agentgateway at the bundle version
supports; unsupported ones fail validation rather than silently doing nothing.

### 4. `model: auto` routing through vSR (M3)

vSR (pinned standalone image) is rendered from the **installed** models and `spec.routing`:
simple → `defaults.fast`, complex/reasoning (vSR complexity and keyword signals) → the large model,
external models only when a rule names them, unhealthy backend → `defaults.fallback`. Explicit
model names bypass vSR. `mode: observe` routes everything to `fast` and records vSR's decision as a
trace attribute (`oaf.route`), so an admin sees what *would* happen before turning it `active`.
Absent backends are never rendered (CLAUDE.md routing rules).

### 5. Growing the cluster: join flow and topologies

- **Join:** `POST /nodes/join-tokens` returns a one-time token (1 h) and
  `curl -fsSL https://<control>:8443/join.sh | sudo bash -s -- --token <t>`. `join.sh` downloads the
  pinned `oaf` binary and the component images **from the control node** (air-gapped works), runs
  preflight, enrols in the cluster PKI (token → node cert), starts `oaf-node`, and reports hardware
  (GPU, memory, disk, serial, both QSFP ports and their link state/peers). Nothing is deployed on
  join.
- **Topologies** (`spec.fabric.topology`), extending today's `single | pair | triangle | two-pairs`:
  - `pair` (option a): unchanged; the new node is cabled port 1↔port 1 (dual rail).
  - `pair-plus-single` (option c): one pair over ConnectX-7, plus a node reached over LAN only. Groups:
    the pair is a TP=2 group; the single runs its own models.
  - option b is not a topology but a **site**: see §6.
- **Recommendation** (`/topology/recommend`): given the pending nodes and their cabling, list the
  options that are physically possible, each with total memory, the catalog models that then fit
  (with profiles), and the routing it would set up; e.g. c) "small model here, GLM-5.3-Flash on the
  pair, complex queries routed to the pair (vSR)".
- **Plan:** the chosen option becomes one plan: nodes, links (subnets allocated), groups, model
  placements, routing rules. Apply runs fabric bring-up (MTU 9000, GID detection, `ib_write_bw`
  per rail), weight fan-out over the fabric, deploy, acceptance.

GLM-5.3-Flash NVFP4 (`nvidia/GLM-5.3-Flash-NVFP4`, ~320B/18B active, ~190 GB) needs the pair. Its
architecture is not in upstream vLLM at our pin; the catalog entry names a GB10 build as its engine
(as `qwen3.8-flash-next-fp4` does) once one is verified on the hardware.

### 6. 1:1 HA (option b) — design, to be reviewed before build

Two complete appliances, **A** (active) and **B** (standby), same models, connected over LAN; users
reach them through an external load balancer (or a VIP) that follows a health endpoint.

- **What fails over:** gateway, models, Garrison console and its data, Langfuse, metrics.
- **Health for the LB:** `GET /healthz/active` on each box answers 200 only on the active one, so an
  LB with a plain HTTP check routes to it; `/healthz` reports liveness.
- **Leader election:** a lease in the control node's store, renewed by A; B takes over after the lease
  expires (default 30 s) **and** A is unreachable on two paths (LAN + a second check), to avoid split
  brain. Manual `oaf ha promote|demote` for planned failover.
- **Config:** `cluster.yaml` and secrets replicate A→B on every apply (oafd push, mTLS); B renders the
  same stack.
- **State:**
  - Postgres (Garrison, Langfuse): streaming replication A→B; B promotes on takeover.
  - Redis/Valkey: queues and caches, rebuilt on B (jobs in flight are retried).
  - ClickHouse (Langfuse traces) and Prometheus/Loki: not replicated in v1; B keeps its own from the
    takeover on. Documented as a gap (traces/metrics before failover stay on A until it returns).
  - Model weights: present on both (fanned out on apply).
- **Singleton work** (Garrison pollers, schedulers, escalations): runs only where the lease is held.
- **Failback:** manual, after A is healthy and has caught up (`oaf ha failback`).

Open points for the review: the second liveness path, whether ClickHouse replication is needed in v1,
and LB product guidance (HAProxy/F5/NGINX health-check examples).

## Build order

1. OAF: `oafd` API skeleton (status, config, models, keys, plans/apply), providers, guardrails.
2. OAF: vSR rendering (§4), observe → active.
3. Garrison: AI Gateway pages (models, providers with the guardrail warning, guardrails, routing,
   keys) on this API, admin-only, changes as approvals.
4. OAF + Garrison: join flow, topology recommend/plan, `pair-plus-single`, the onboarding wizard.
   Needs the owner's two other boxes.
5. HA (§6) after its review; needs two boxes on the LAN and an LB or VIP.

## Consequences

- One API serves the CLI's capabilities to any app; Garrison stays a client and OAF stays usable
  alone.
- External models and guardrails become declarative and reviewable; the dangerous case is visible.
- New topology `pair-plus-single` and the HA "site" add surface to validation and acceptance tests.
- HA is a large, separate milestone with its own failure testing.
