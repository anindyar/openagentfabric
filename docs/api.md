# openagentfabric management API

This is the reference for `oafd`, the management API of openagentfabric (OAF). It is written for the
team that builds an app on top of OAF: an app console that shows the cluster, adds models, edits
routing and issues keys without anyone editing files by hand. The machine-readable description is
[openapi.yaml](openapi.yaml) (OpenAPI 3.1). The operator's view of the same feature is in the
[user guide](user-guide.md#management-api-oafd); the design is in
[ADR-0002](adr/0002-management-api-gateway-cluster.md).

Contents:
1. [Overview](#1-overview)
2. [Authentication](#2-authentication)
3. [Conventions](#3-conventions)
4. [The two-step write model](#4-the-two-step-write-model)
5. [Licence acceptance](#5-licence-acceptance)
6. [Events](#6-events)
7. [Endpoints](#7-endpoints)
8. [The data plane](#8-the-data-plane)
9. [How an app should integrate](#9-how-an-app-should-integrate)
10. [Not implemented](#10-not-implemented)

---

## 1. Overview

`oafd` is the role of the single `oaf` binary that `oaf serve` runs on the control node
(`oaf-api.service`, port 8443 by default). It is an HTTPS API that:

- reads `cluster.yaml` (the one declarative source of truth) and the model catalog,
- turns a change request into an edited `cluster.yaml`, validates it and returns it as a **plan**
  with a diff, and
- applies a plan by writing `cluster.yaml` and running the same reconciler as `oaf apply`.

`oafd` holds no state of its own beyond the list of plans in flight, which it keeps for an hour.
Everything a request changes lives in `cluster.yaml` (or, for secrets, in the OAF secrets store, and
only as references in the file). Anything you can do through the API you can also do by editing the
file and running `oaf apply`, and the two stay consistent.

The management API is **not on the request path**. Your app's model calls go to the gateway
(section 8); `oafd` can be down and inference keeps working.

---

## 2. Authentication

Every request under `/api/v1/` needs a management token:

```
Authorization: Bearer <management token>
```

One token belongs to one app. An admin issues it on the control node:

```sh
sudo oaf keys issue myapp --admin --host oaf.example.internal
```

Besides the gateway settings for the app, this prints:

```
OAF_API_URL=https://oaf.example.internal:8443
OAF_API_TOKEN=<management token>
OAF_API_CERT_SHA256=<SHA-256 of the API's certificate, hex>
```

The token is the secret `oaf-admin-<app>` in the OAF secrets store. Any `oaf-admin-*` secret is
accepted; the app name is recorded as the author of the plans it makes (the plan's `app` field and
the `oafd` log). The comparison is done on digests in constant time. A failed attempt is answered
after a delay of 500 ms, with `401`:

```json
{"error": "a management token is required (oaf keys issue <app> --admin)"}
```

Rules for the app:

- **Keep the token on your server.** The management API can change routing, add external providers
  and issue keys. Never send the token to a browser or a mobile client. Your backend calls `oafd`
  and decides which of your users may ask for which change.
- The API is HTTPS only. The certificate is issued by the cluster CA when the control node has one,
  else it is self-signed. Pin `OAF_API_CERT_SHA256` (the SHA-256 of the DER certificate) in your
  HTTP client rather than turning verification off.
- One unauthenticated endpoint exists: `GET /healthz` returns `200` with no body.
- There are no scopes. A token can use every endpoint in this document.

---

## 3. Conventions

- Base path `/api/v1`. Bodies and replies are JSON (`Content-Type: application/json`), field names in
  `snake_case`. Replies carry `Cache-Control: no-store`.
- A request body is limited to 1 MiB. Most endpoints reject unknown fields with `400`.
- Errors are `{"error": "<message>"}` with a status code. Other shapes are noted per endpoint.
  Common codes: `400` bad request, `401` no or wrong token, `404` unknown name or plan, `409`
  conflict, `422` the change does not validate, `501` a service is not configured, `502` an upstream
  step failed.
- Names (deployments, providers, guardrail policies, keys) are used as path segments; use short
  lowercase names with letters, digits and `-`.
- The examples below use `$OAF_API_URL` and `$OAF_API_TOKEN` from section 2, and
  `-H "Authorization: Bearer $OAF_API_TOKEN"` is shortened to `$AUTH`:

  ```sh
  AUTH="Authorization: Bearer $OAF_API_TOKEN"
  # add --cacert <cluster CA> to pin trust, or --pinnedpubkey for the key pin
  ```

---

## 4. The two-step write model

Every change to `cluster.yaml` is a **plan** first. A `POST`, `PUT` or `DELETE` that changes the
cluster does not change anything yet. It answers with a plan:

```json
{
  "plan_id": "9f2c41d07a6be315",
  "summary": "Deploy med-fast (qwen3-30b-a3b-instruct-2507-fp8)",
  "app": "myapp",
  "status": "planned",
  "diff": "--- cluster.yaml\n+++ cluster.yaml (planned)\n@@ -40,3 +40,8 @@\n ...",
  "warnings": [{"path": "spec.models", "message": "..."}],
  "errors": [],
  "created_at": "2026-11-02T10:15:00Z",
  "expires_at": "2026-11-02T11:15:00Z"
}
```

| Field | Meaning |
|---|---|
| `plan_id` | Opaque id, 16 hex characters. |
| `summary` | One line for an approval prompt. `" (no change)"` is appended when the diff is empty. |
| `app` | The app whose token made the plan. |
| `status` | `planned`, `applying`, `applied` or `failed` (below). |
| `diff` | Unified diff of `cluster.yaml` (three lines of context). Empty if nothing changes. |
| `warnings` | Validation findings that do not stop an apply. Each is `{path, message}`. |
| `errors` | Validation findings that do. Same shape. |
| `needs_acceptance` | Licences to accept first (section 5). Present only when there are some. |
| `steps` | Progress lines of an apply: `{name, status: running\|done\|failed}`. |
| `error` | Why a plan is `failed`. |
| `created_at`, `expires_at`, `applied_at` | RFC 3339 times. `applied_at` is set when the apply finished. |

The reply code is `201` when the plan can be applied (warnings do not matter) and `422` when it has
`errors`. The body is a plan in both cases. A `422` plan has `status: "planned"` and cannot be
applied: applying it answers `422` with `{"error": ..., "errors": [...]}`. Show the `errors` to a
person; the usual fix is a new request.

Review the plan, then apply it:

```sh
curl -sS -X POST "$OAF_API_URL/api/v1/plans/9f2c41d07a6be315/apply" -H "$AUTH"
```

`202` means the apply has started in the background; the body is the plan with `status: "applying"`.
Follow it with `GET /api/v1/plans/{id}` or the event stream (section 6) until `status` is `applied` or
`failed`. Before it writes anything, apply renders the new `cluster.yaml`; if that fails, the reply
is `422` `{"error": "not applied, cluster.yaml unchanged: ...", "plan_id": ...}` and the file is
untouched.

Rules to rely on:

- **Optimistic concurrency.** A plan records the hash of the `cluster.yaml` it was made against. If
  the file changed since (another plan, a person at the CLI), applying answers `409` with
  `"cluster.yaml changed since this plan was made; make a new plan"` and the plan becomes `failed`.
  Make a new plan and show the new diff.
- **One apply at a time.** While a plan is `applying`, applying another answers `409`
  (`"another plan is being applied: <id>"`). Applying a plan twice answers `409` (`"the plan is
  applied"`).
- **Plans expire.** An unapplied plan can be applied for one hour (`expires_at`); after that it reads
  as `failed`. Plans are forgotten after 24 hours, and after `oafd` restarts a known plan reads as
  `failed` with `"the plan expired (oafd restarted)"`. An unknown id is `404`.
- **A failed apply leaves the change in `cluster.yaml`.** The write comes first, then the reconcile.
  If the reconcile fails (`status: "failed"`, `error` explains), fix it with a new plan or re-run
  `oaf apply`; the plan is not rolled back.
- **Not everything is a plan.** `POST /keys` and `POST /keys/{name}/rotate` act immediately
  (section 7.6).
- **Models start downloading at plan time.** Adding or replacing a model whose plan has no errors
  starts downloading its weights on its nodes right away, before approval, so it is ready sooner.
  Progress is in `GET /status` `downloads`. A plan with errors does not start a download.
- Providers' `api_key` values are stored when the plan is *made*, not when it is applied, and are
  never returned.

---

## 5. Licence acceptance

Some components and models have terms a person must read (the vLLM engine image ships NVIDIA's CUDA
libraries; some models are gated). They are recorded in `spec.licenseAcceptances` with who accepted
and when. A change that deploys something with unaccepted terms is refused at **plan** time, so it
cannot fail halfway through an apply. The reply is `422` with the usual plan body plus
`needs_acceptance`, and one error per licence:

```json
{
  "plan_id": "4b7e0c9a12d3f685",
  "summary": "Deploy big (ornith-1.5-122b-nvfp4)",
  "status": "planned",
  "errors": [
    {"path": "spec.licenseAcceptances",
     "message": "image/vllm needs NVIDIA CUDA Toolkit EULA accepted first (https://docs.nvidia.com/cuda/eula/index.html): accept it in the console or POST /api/v1/licenses/accept"}
  ],
  "needs_acceptance": [
    {"subject": "image/vllm", "kind": "image", "title": "NVIDIA CUDA Toolkit EULA",
     "for": "vllm engine image (for big)", "url": "https://docs.nvidia.com/cuda/eula/index.html"}
  ]
}
```

`subject` is `image/<component>` or `model/<catalog id>`. `GET /api/v1/status` lists
`licenses_needed` for anything the current `cluster.yaml` deploys without an acceptance.

The flow for an app:

1. Show each `needs_acceptance` entry (`title`, `for`, and a link to `url`) to a person who is
   entitled to accept it. Do not accept on a user's behalf without that person's action.
2. Send the subjects and the person's identity, `POST /api/v1/licenses/accept`:

   ```sh
   curl -sS -X POST "$OAF_API_URL/api/v1/licenses/accept" -H "$AUTH" \
     -H 'Content-Type: application/json' \
     -d '{"subjects": ["image/vllm"], "accepted_by": "ana@example.org"}'
   ```

   `accepted_by` is required and is recorded in `cluster.yaml` with the time and the terms URL.
3. The reply is a plan (`201`) that adds the acceptances. Apply it like any plan. It is a
   *record-only* plan: it can be applied while another plan is applying, it is rebuilt on the current
   `cluster.yaml` if that changed (so it does not `409`), and it reconciles only if something already
   deployed was waiting on it. In that record-only case the apply answers `200` with
   `status: "applied"` instead of `202`.
4. Repeat the original request. It now plans without `needs_acceptance`.

`400` is returned when `subjects` is empty, `accepted_by` is empty, or a subject is not a licence
that this bundle or catalog asks to accept. A subject that is already accepted is not duplicated; if
all are accepted the plan's summary is `"licences already accepted"`.

---

## 6. Events

`GET /api/v1/events` is a server-sent event stream (`Content-Type: text/event-stream`). Open it once
and keep it open. It starts with a comment line (`: oafd events`) and sends `: ping` every 20 seconds
to keep proxies from closing it. Each event is one `data:` line holding a JSON object:

```
data: {"type":"plan","plan_id":"9f2c41d07a6be315","status":"applying","message":"update spark-1/gateway","at":"2026-11-02T10:16:04Z"}
```

| Field | Meaning |
|---|---|
| `type` | `plan` (apply progress), `fabric` and `link` (fabric bring-up and cable tests during an apply), `join` and `preflight` (a new box joining). |
| `plan_id` | The plan, for `plan`, `fabric` and `link` events from an apply. |
| `status` | For plan events `applying`, `applied` or `failed`. For join events, e.g. `token` or `pending`. |
| `message` | A human-readable progress line. |
| `at` | RFC 3339 UTC time. |

An apply sends one event when it starts (`applying: <summary>`), one per line of reconciler output,
and a final event with `status` `applied` (`applied: <summary>`) or `failed` (the error).

Events are a live feed: there is no replay and no event id, and a subscriber that reads too slowly
loses events. Treat the stream as a hint and use `GET /plans/{id}` and `GET /status` as the truth,
for example after a reconnect.

```sh
curl -sSN "$OAF_API_URL/api/v1/events" -H "$AUTH"
```

---

## 7. Endpoints

34 endpoints, all under `/api/v1` and all needing the token. Endpoints marked *plan* answer
`201`/`422` with the plan of section 4.

| Group | Endpoints |
|---|---|
| [7.1 Status, config, catalog](#71-status-config-catalog) | `GET /status`, `GET /config`, `GET /catalog` |
| [7.2 Models](#72-models) | `GET /models`, `POST /models`, `PUT /models/{name}`, `DELETE /models/{name}`, `PUT /models/{name}/guardrails`, `POST /models/{name}/replace` |
| [7.3 Providers](#73-providers) | `GET /providers`, `POST /providers`, `PUT /providers/{name}`, `DELETE /providers/{name}` |
| [7.4 Guardrails](#74-guardrails) | `GET /guardrails`, `POST /guardrails`, `PUT /guardrails/{name}`, `DELETE /guardrails/{name}` |
| [7.5 Routing](#75-routing) | `GET /routing`, `PUT /routing` |
| [7.6 Keys](#76-keys) | `GET /keys`, `POST /keys`, `POST /keys/{name}/rotate` |
| [7.7 Plans and licences](#77-plans-and-licences) | `GET /plans/{id}`, `POST /plans/{id}/apply`, `POST /licenses/accept` |
| [7.8 Nodes, joining, topology](#78-nodes-joining-topology) | `GET /nodes`, `PUT /nodes/{name}/headless`, `POST /nodes/join-tokens`, `GET /nodes/join-tokens`, `GET /nodes/pending`, `DELETE /nodes/pending/{name}`, `POST /topology/recommend`, `POST /topology/plan` |
| [7.9 Events](#79-events) | `GET /events` |

Plus `GET /healthz` (no token).

### 7.1 Status, config, catalog

#### GET /status

What `oaf status` prints: nodes, groups, deployments and components with their health.

```sh
curl -sS "$OAF_API_URL/api/v1/status" -H "$AUTH"
```

Response `200`:

| Field | Meaning |
|---|---|
| `cluster` | Cluster name. |
| `topology` | `single`, `pair`, `triangle`, `two-pairs` or `pair-plus-single`. |
| `nodes[]` | `name`, `address`, `roles[]`, `state` (`joined`, `pending`, `joining`, `offline`), `health`, `hardware` (`gpu`, `memory_gib`, `memory_free_gib`, `disk_free_gib`, `serial`, `os`, `arch`), `ports[]` (`port`, `link` `up`/`down`, `peer`, `speed_gbps`, `rails[]` with `netdev`, `ip`, `bw_gbps`, `status`, `rdma_dev`), `preflight[]` (`name`, `status` `pass`/`warn`/`fail`, `summary`), `version`, `joined_at`. |
| `groups[]` | Placement groups: `name`, `nodes[]` (the boxes a multi-node model runs across). |
| `deployments[]` | One per model deployment: `name`, `catalog_ref`, `task` (`chat`, `embedding`), `placement` (`node` or `group`), `profile`, `nodes[]`, `enabled`, `gpu_memory_utilization`, `max_model_len`, `guardrails[]`, `state`, `health`, and the console's view: `phase` (`queued`, `downloading`, `starting`, `loading`, `ready`, `failed`, `removing`, `disabled`), `progress` (0-100 while loading), `detail`, `error`, `since`, `restarts`. A model removed from `cluster.yaml` but still running shows with `phase: "removing"`. |
| `components[]` | Stack containers: `name` (`project/service`), `node`, `state`, `health`. `state` is `unreachable` or `missing` when the node agent cannot be reached or the project is absent. |
| `licenses_needed[]` | `needs_acceptance` entries (section 5) for what `cluster.yaml` deploys without an acceptance. |
| `applying` | `{plan_id, summary, since}` while a plan is being applied; absent otherwise. |
| `downloads[]` | Weights on nodes: `node`, `model_id`, `state` (`downloading`, `complete`, `failed`), `bytes_done`, `bytes_total`, `bytes_per_second`, `error`, `started_at`. Includes models not deployed yet. |

Errors: `502` when the status cannot be gathered, `501` if status is not configured.

#### GET /config

`cluster.yaml` as text, its hash, and the rendered gateway config (read-only). Secrets appear as
references only.

```sh
curl -sS "$OAF_API_URL/api/v1/config" -H "$AUTH"
```

Response `200`: `cluster_yaml` (string), `hash` (SHA-256 of the file, hex), and either
`gateway_config` (the agentgateway `config.yaml` as OAF would render it) or `gateway_config_error`.
Do not edit and re-post this file; change the cluster through the typed endpoints.

#### GET /catalog

The models OAF can deploy, from `catalog/models.yaml`.

```sh
curl -sS "$OAF_API_URL/api/v1/catalog" -H "$AUTH"
```

Response `200` `{"models": [...]}`. Each model: `id` (use it as `catalog_ref`), `name`, `task`
(`chat` or `embedding`), `memory_gib` (weights), `quantization`, `context_window` (the largest
profile `max_model_len`), `profiles[]` (`name`, `nodes`, `gpu_memory_utilization`, `max_model_len`),
`license`, `hf_repo`, `classes[]`, `offered` (always true today), `recommended` (a companion model
that fits beside a large model the cluster runs) and `note` (why).

### 7.2 Models

A model *deployment* is an entry under `spec.models`: a served name, a catalog model, and where it
runs.

#### GET /models

```sh
curl -sS "$OAF_API_URL/api/v1/models" -H "$AUTH"
```

Response `200` `{"models": [...]}` with the fields of a `deployments[]` entry in `/status` except the
live ones (`state` is `configured` or `disabled`; there is no `phase`). Use `/status` for health.

Each deployment lists its LoRA adapters (also in `/status`), `[]` when it has none:

```json
"adapters": [{"name": "spetaar-ddx", "path": "/var/lib/oaf/adapters/spetaar-ddx", "rank": 16,
              "entitlement": "ddx", "guardrails": [], "served": false}]
```

`served` is false while the deployment is disabled or the adapter waits for its entitlement (no
entitlement is verified yet, so an entitled adapter is never served). `guardrails` are the effective
ones: the deployment's unless the adapter sets its own. A served adapter is a model name of its own
for clients, keys and routing rules. Adapters are read-only in the API: they are set in
`cluster.yaml` (docs/user-guide.md, LoRA adapters).

#### POST /models (plan)

Deploy a catalog model.

| Body field | |
|---|---|
| `name` | Required. The name clients send as `model`. |
| `catalog_ref` | Required. An `id` from `/catalog`. |
| `placement` | `{"node": "<node>"}` or `{"group": "<group>"}`. Default: the first node with the inference role. |
| `profile` | Catalog profile name. Default: the one whose node count matches the placement. |
| `gpu_memory_utilization` | Share of unified memory, e.g. `0.30`. Default: the profile's. |
| `max_model_len` | Context window. Default: the profile's. |
| `guardrails` | Policy names to attach. |

```sh
curl -sS -X POST "$OAF_API_URL/api/v1/models" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"med-fast","catalog_ref":"qwen3-30b-a3b-instruct-2507-fp8","placement":{"node":"spark-1"}}'
```

Errors: `400` unknown `catalog_ref` or bad body, `409` a deployment with that name exists, `422` the
plan has errors (for example the memory shares on a node would pass 0.90, or a licence is needed).

#### PUT /models/{name} (plan)

Change serving settings. Send any of `max_model_len` (`0` restores the profile's), 
`gpu_memory_utilization`, `enabled` (`false` stops and removes the container; weights stay on disk).
Absent fields stay as they are.

```sh
curl -sS -X PUT "$OAF_API_URL/api/v1/models/med-fast" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"max_model_len": 16384}'
```

Errors: `404` no such deployment, `400` none of the three fields sent.

#### DELETE /models/{name} (plan)

Remove a deployment. `404` if it does not exist.

```sh
curl -sS -X DELETE "$OAF_API_URL/api/v1/models/med-fast" -H "$AUTH"
```

#### PUT /models/{name}/guardrails (plan)

Replace the policies attached to a deployment: `{"guardrails": ["basic"]}` (an empty list detaches
all). `404` if the deployment does not exist. If the result does not validate (for example an unknown
policy name), the plan is `422`.

#### POST /models/{name}/replace (plan)

Swap the model behind a deployment in one change; only the end state has to fit, so a box's model can
be replaced by a bigger one.

| Body field | |
|---|---|
| `catalog_ref` | Required. The new catalog model. |
| `name` | New served name. Empty keeps the current name: routing, keys and clients keep working, and the old model serves until the new weights are downloaded. With a new name the old deployment is removed and routing and app keys move to the new one. |
| `placement` | Moves it. Empty keeps the current node or group. |
| `gpu_memory_utilization`, `max_model_len` | As in `POST /models`. |

Guardrails and `enabled` carry over; LoRA adapters carry over only when the catalog model stays the
same (they load on the base they were trained for). Errors: `404` no such deployment, `400` unknown `catalog_ref`,
`409` the new name is taken. The download starts when the plan is made.

### 7.3 Providers

External providers (a hosted model API). Off by default: each provider needs `allow_egress: true`,
and a provider without a guardrail policy plans with a warning:
`External model without guardrails: prompts, code and data leave the appliance unfiltered. This is
extremely dangerous; use with care.` Show it to the user.

#### GET /providers

```sh
curl -sS "$OAF_API_URL/api/v1/providers" -H "$AUTH"
```

Response `200`: `providers[]` and `no_guardrail_warning` (the text above). A provider:

| Field | |
|---|---|
| `name` | Provider name. |
| `type` | `openai`, `anthropic`, `gemini`, `azure-openai` or `openai-compatible`. |
| `endpoint` | Base URL; for `openai-compatible`. |
| `azure` | `{resource_name, api_version}` for `azure-openai`. |
| `models[]` | `{name, upstream}`: the name clients send, and the provider's own name if different. |
| `secret_ref` | The secret holding the key. Default `provider-<name>`. |
| `guardrails[]` | Attached policy names. |
| `allow_egress` | Must be true to use the provider. |
| `has_key` | Whether the secret exists. The key is never returned. |

#### POST /providers and PUT /providers/{name} (plan)

`POST` creates and `PUT` replaces a provider. The body has the fields above plus `api_key`, which is
stored as the provider's secret when the plan is made and is never echoed. A provider key is never
generated: a provider with a `secret_ref` and no stored key makes the plan `422` ("has no API key yet:
send api_key with the provider"). `PUT` keeps the existing `secret_ref` if you omit it.

```sh
curl -sS -X POST "$OAF_API_URL/api/v1/providers" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"openai","type":"openai","allow_egress":true,"api_key":"'"$PROVIDER_KEY"'",
       "models":[{"name":"gpt-4o"}],"guardrails":["basic"]}'
```

Errors: `409` on `POST` if the name exists, `404` on `PUT` if it does not, `400` if the path name and
body name differ (a provider cannot be renamed), `400` for a malformed provider.

#### DELETE /providers/{name} (plan)

`404` if unknown. If the result no longer validates (for example a routing rule still names one of
its models), the plan is `422`.

### 7.4 Guardrails

Guardrail policies are named lists of rules applied to a provider's or deployment's requests and
responses by the gateway.

#### GET /guardrails

```sh
curl -sS "$OAF_API_URL/api/v1/guardrails" -H "$AUTH"
```

Response `200`: `guardrails[]`, `builtins[]` (the detector names the pinned gateway has: `ssn`,
`creditCard`, `phoneNumber`, `email`, `caSin`) and `templates[]` (the `basic` policy). A policy:

| Field | |
|---|---|
| `name`, `description` | |
| `message` | What a client sees when a request is rejected (HTTP 403). |
| `rules[]` | `{builtin \| regex \| webhook, action, on, name}`. `on` is `request` or `response`. `action` is `reject`, `mask` or `audit` (default `reject`). `webhook` is a `host:port` of a guard service and has no action. |
| `attach` | `{providers[], models[]}` it is attached to (read-only here; set it on write). |

On write, builtin spellings `phone`, `phone_number`, `credit_card`, `creditcard`, `ca_sin` and
`casin` are accepted and stored as the pinned names. An unknown detector or a regex that does not
compile makes the plan `422`.

#### POST /guardrails and PUT /guardrails/{name} (plan)

Create or replace a policy, and attach it in the same plan with
`"attach": {"providers": [...], "models": [...]}`. `{"template": "basic"}` uses the recommended policy
(rejects prompts with credentials and private keys, masks emails, phone numbers and card numbers in
responses); `name` then renames the copy.

```sh
curl -sS -X POST "$OAF_API_URL/api/v1/guardrails" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"template":"basic","attach":{"providers":["openai"]}}'
```

Errors: `409` on `POST` if the policy exists (not for a template), `400` unknown template or path/body
name mismatch, `400` if `attach.keys` is used (limits for keys are set on the key), `404` an attached
provider or model does not exist.

#### DELETE /guardrails/{name} (plan)

Removes the policy and detaches it from every provider and model in the same plan. `404` if unknown.
Removing the last policy from a provider brings the no-guardrail warning back.

### 7.5 Routing

What `model: auto` does, and the defaults. Routing is rendered into the gateway and the semantic
router from the models actually deployed; a rule never routes to a backend that is absent.

#### GET /routing

```sh
curl -sS "$OAF_API_URL/api/v1/routing" -H "$AUTH"
```

Response `200`:

```json
{
  "mode": "observe",
  "defaults": {"fast": "med-fast", "fallback": "med-fast"},
  "rules": [
    {"name": "think", "target": "med-large",
     "match": {"signals": [{"type": "keyword", "keywords": ["step by step", "prove"]}]},
     "requires_entitlement": ""}
  ]
}
```

`mode` is `observe` (everything goes to `defaults.fast`; the router's decision is only recorded as a
trace attribute) or `active` (rules are applied). A rule fires when **any** of its signals fires.
A signal has a `type` of `keyword` (`keywords[]`, `case_sensitive`), `context` (`min_tokens`),
`reasoning` (`threshold`; needs the router's encoder artifact) or `domain` (`domains[]`; needs a
classifier that no bundle pins yet, so it is not rendered). `requires_entitlement` names an
entitlement feature that must be granted for the rule to be rendered.

#### PUT /routing (plan)

Replaces `mode`, `defaults` and `rules` together; send all three. An empty `mode` or `rules` removes
that section.

```sh
curl -sS -X PUT "$OAF_API_URL/api/v1/routing" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"mode":"active","defaults":{"fast":"med-fast","fallback":"med-fast"},"rules":[]}'
```

Errors: `400` malformed body or an edit that cannot be made; `422` the plan does not validate (for
example a target that is not a chat deployment, or an unknown signal type).

### 7.6 Keys

App keys are the credentials for the gateway (section 8). These two writes are **not plans**: they
take effect at once because the key service re-renders and restarts the gateway itself.

#### GET /keys

```sh
curl -sS "$OAF_API_URL/api/v1/keys" -H "$AUTH"
```

Response `200` `{"keys": [{"name", "models"?, "requests_per_minute"?, "fingerprint"?}]}`.
`fingerprint` is the first 12 hex characters of the SHA-256 of the key, to tell keys apart. The key
itself is never listed. `models` empty means all models.

#### POST /keys

Issue a key, `{"name": "<key>", "models": ["a", "b"]}` (`models` optional; omit for all).

```sh
curl -sS -X POST "$OAF_API_URL/api/v1/keys" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"clinical-app","models":["med-fast"]}'
```

Response `200`: `{"name", "key", "models", "fingerprint"}`. **The `key` is in this reply only**;
store it in your secret manager. Issuing is idempotent: an existing key of that name is returned
unchanged. Gateway requests in flight while it is re-applied can fail; retry them.

Errors: `400` missing `name`, `501` key service not configured, `502` the gateway could not be
applied (the reply has the reason).

#### POST /keys/{name}/rotate

Replace the key and return the new one; the old one stops working.

```sh
curl -sS -X POST "$OAF_API_URL/api/v1/keys/clinical-app/rotate" -H "$AUTH"
```

Response `200`: `{"name", "key", "fingerprint"}`. Errors: `429` rotated less than 30 seconds ago,
`502` failed (an unknown name is reported here as `cluster.yaml has no key "<name>"`; if the gateway
cannot be applied the old key is restored and stays valid), `501` key service not configured.

There is no endpoint to revoke a key; see [section 10](#10-not-implemented).

### 7.7 Plans and licences

#### GET /plans/{id}

Follow a plan. Response `200` is the plan of section 4, with `steps` filled during and after an
apply. `404` for an unknown id.

```sh
curl -sS "$OAF_API_URL/api/v1/plans/9f2c41d07a6be315" -H "$AUTH"
```

#### POST /plans/{id}/apply

Apply a plan. No body.

| Code | Meaning |
|---|---|
| `202` | Started; body is the plan with `status: "applying"`. |
| `200` | A record-only licence plan was applied at once. |
| `404` | Unknown or forgotten plan. |
| `409` | The plan is not `planned` (already applied, failed or expired), another plan is applying, or `cluster.yaml` changed since the plan was made. |
| `422` | The plan has `errors` (`{"error", "errors"}`), or the change does not render (`{"error", "plan_id"}`; `cluster.yaml` unchanged). |
| `500` | `cluster.yaml` could not be read or written. |

#### POST /licenses/accept

See section 5. Body `{"subjects": [...], "accepted_by": "..."}`. Reply: a plan, `201` (or `422` if
something else is wrong with the result). Errors: `400`.

### 7.8 Nodes, joining, topology

These endpoints are available when the control node has the cluster CA key (`oaf pki init`); without
it they answer `404`.

#### GET /nodes

Nodes in `cluster.yaml` and boxes that joined but are not placed yet.

```sh
curl -sS "$OAF_API_URL/api/v1/nodes" -H "$AUTH"
```

Response `200` `{"nodes": [...]}`. A node: `name`, `address`, `roles[]`, `state` (`joined`, `pending`,
`joining`, `offline`), `health`, `hardware` (`gpu`, `memory_gib`, `memory_free_gib`, `disk_free_gib`,
`serial`, `os`), `ports[]` and `preflight[]` as in `/status`, `joined_at` (epoch seconds), `desktop`
(`mode` `graphical`/`headless`, `display_manager_active`, `estimated_mib`) and `headless` (the
`cluster.yaml` setting, if set). Pending boxes have `state: "pending"` and no roles.

#### PUT /nodes/{name}/headless (plan)

Run a box without its desktop GUI, which frees memory for models, or bring it back:
`{"headless": true}`.

```sh
curl -sS -X PUT "$OAF_API_URL/api/v1/nodes/spark-1/headless" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"headless": true}'
```

Errors: `400` if `headless` is missing, `404` unknown node.

#### POST /nodes/join-tokens

Create a one-time token and the command to run on each new box.

| Body field | |
|---|---|
| `count` | Boxes that may use the token, 1 to 8. Default 1. |
| `ttl_minutes` | Validity, 1 to 1440. Default 60. |
| `label` | Free text. |

The body may be empty. Reply `201`: `id`, `token` (shown once), `count`, `expires_at` (epoch seconds)
and `command`, a shell line for the new box that pins the control node's TLS key. The token travels in
the environment, not on the command line. Errors: `400` for values out of range.

```sh
curl -sS -X POST "$OAF_API_URL/api/v1/nodes/join-tokens" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"count": 2}'
```

#### GET /nodes/join-tokens

`{"tokens": [{"id", "label", "count", "uses", "expires_at", "used_by": [node names]}]}`. Token values
are not listed.

#### GET /nodes/pending

`{"nodes": [...]}`: boxes that enrolled and wait for a topology, with their reported hardware
(hostname, arch, GPU, memory, disk, serial, QSFP ports and neighbours, preflight results).

#### DELETE /nodes/pending/{name}

Forget a pending box. `204` on success, `404` if there is no such pending box.

#### POST /topology/recommend

Given the boxes that joined (or placeholders), list the ways to grow and what each enables.

| Body field | |
|---|---|
| `new_nodes` | Required, 1 or 2 (OAF grows in pairs; there is no switch). |
| `cabling` | `cx7` (the new box is cabled to this one over ConnectX-7), `cx7-pair` (two new boxes cabled to each other), `lan` or `unknown`. Picks which option is `recommended`. |

Reply `200` `{"options": [...]}`. An option: `id` (`a`, `b`, `c`), `title`, `summary`, `topology`,
`nodes_needed`, `possible`, `reason`, `total_memory_gib`, `models[]` (`id`, `placement`, `note`),
`routing[]`, `ha`, `coming_next`, `recommended`, `cabling[]` (`from`, `to`, `kind` `cx7`/`lan`,
`done`, `text`) and `new_nodes[]`. Option `b` (high availability over LAN) is described but has
`possible: false` and `coming_next: true`.

#### POST /topology/plan

Turn a chosen option into a plan (nodes, links, groups, models, routing).

| Body field | |
|---|---|
| `option` | `a` or `pair`; `c` or `pair-plus-single`. |
| `nodes` | Names of pending boxes to use. Default: all pending. |
| `model`, `name` | Catalog model to place on the new capacity and the name to serve it as. |
| `no_model` | Add the boxes without a model. |
| `headless` | Run the new boxes without the desktop GUI. |

Reply: `{"plan_id", "summary", "diff", "warnings": [string], "errors": [string], "needs_acceptance"}`,
`200` if it has no errors and `422` otherwise. Apply `plan_id` with `POST /plans/{id}/apply`. Note
that `warnings` and `errors` are plain strings here, not `{path, message}` objects. `400` is returned
for a node that has not joined, and `422` with only `errors` when the option cannot be planned (for
example option `b`, or no catalog model fits).

#### Node-facing endpoints

`/join.sh` and `/api/v1/join/*` (`binary`, `images`, `images/{name}`, `enrol`) are used by `oaf join`
on a new box and authenticate with a join token, not a management token. An app does not call them.

### 7.9 Events

`GET /events`: see section 6.

---

## 8. The data plane

The management API never carries inference traffic. An app that wants model output:

1. Has a **gateway key** from `POST /keys` (or `oaf keys issue <app>`).
2. Calls the **agentgateway** OpenAI-compatible endpoint on the gateway node, usually
   `http://<gateway>:4000/v1`, with `Authorization: Bearer <gateway key>`:

   ```sh
   curl http://oaf.example.internal:4000/v1/chat/completions \
     -H "Authorization: Bearer $GATEWAY_KEY" -H 'Content-Type: application/json' \
     -d '{"model":"med-fast","messages":[{"role":"user","content":"Hello"}]}'
   ```

   `model` is a deployment name (or a provider model name). `model: auto` is routed by the semantic
   router according to `/routing`; a named model is never re-routed. Any OpenAI SDK works with the
   base URL and key set. Embedding deployments serve `/v1/embeddings`.
3. Can label calls with the headers `X-OAF-User`, `X-OAF-Session` and `X-OAF-Step`. They only label
   traces; they grant nothing.

Every gateway call is traced to **Langfuse** (project named after the cluster) with latency, token
counts and the serving model. An app that wants its own spans in the same trace sends them to
Langfuse and passes a W3C `traceparent` header on each gateway call. `oaf keys issue` prints
`LANGFUSE_HOST`, `LANGFUSE_PUBLIC_KEY` and `LANGFUSE_SECRET_KEY` for this; the same keys read traces
back through Langfuse's public API. Prompt and response text are recorded only when
`observability.langfuse.captureContent` is true.

A key can also rotate itself without the management token: `POST http://<control>:7702/v1/keys/rotate`
with `Authorization: Bearer <current key>` returns `{"name", "key"}` and retires the old key (at most
once every 30 seconds). Use this for a running app that holds only its gateway key.

If `oafd` is stopped, requests with a valid key keep working. Only changes (and status) wait.

---

## 9. How an app should integrate

1. **Get credentials** on the control node: `sudo oaf keys issue myapp --admin`. Put
   `OAF_API_URL`, `OAF_API_TOKEN` and `OAF_API_CERT_SHA256` in your server's secret store, never in
   a browser or a repository.
2. **Pin TLS** with the certificate fingerprint. Check `GET /healthz` at startup.
3. **Read before writing:** `GET /status`, `/catalog`, `/models`, `/routing`, `/providers`.
   Render from `/status` (health, `phase`, `downloads`); it is the live view.
4. **Treat every change as propose, review, apply.** Call the write endpoint, show `summary`, `diff`,
   `warnings` and `errors` to a person with authority, and call `POST /plans/{id}/apply` only on their
   approval. Do not apply a plan with `errors` (it will be refused anyway).
5. **Handle the three plan outcomes:** `422` (show `errors`; for `needs_acceptance`, run the licence
   flow of section 5), `409` on apply (make a new plan and show the new diff), `202` (watch it).
6. **Track the apply** with `GET /events` plus `GET /plans/{id}`. An apply can take minutes (weights
   download, containers start); the deployment is serving when its `phase` is `ready`.
7. **Treat secrets carefully.** The only responses that carry a secret are `POST /keys` and
   `POST /keys/{name}/rotate`; store the key at once. Send provider `api_key` once; read `has_key`
   afterwards. Do not log request bodies for those calls.
8. **Give each app and environment its own gateway key**, restrict it to the models it needs, and
   keep it out of end-user clients.
9. **Retry gateway calls** that fail around a key issue or rotation; the gateway restarts.
10. **Expect concurrent humans.** Another admin may change `cluster.yaml` at any time. Never cache a
    plan beyond its hour, and re-read before showing a diff.
11. Calls to `oafd` are for administration. Do not put them in a user-facing request path.

---

## 10. Not implemented

ADR-0002 lists these; the code does not have them:

- `DELETE /keys` (revoking a key). Rotate it, or remove it from `cluster.yaml`.
- A single `POST /api/v1/apply {plan_id}`; apply is `POST /plans/{id}/apply`.
- Separate `POST .../plan` calls; each write endpoint returns its plan directly.
- Guardrail moderation endpoints and per-key limits through the API (a key's `requests_per_minute`
  is shown by `GET /keys` but is set in `cluster.yaml`).
- Topology option `b` (high availability over LAN) cannot be planned yet.
- Writing LoRA adapters through the API (adding, removing, or changing an adapter on a deployment).
  `GET /models` and `/status` list them; they are edited in `cluster.yaml`.
