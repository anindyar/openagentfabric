---
name: oaf-consumer
description: How to build an app on Open Agent Fabric (OAF), the open-source AI platform for NVIDIA DGX Spark. Use when code in this project calls an OAF gateway, issues or rotates gateway keys, deploys or swaps models, edits routing or guardrails, or reads OAF traces. Covers the two surfaces (inference gateway and management API) and the rules that keep an integration safe.
---

# Building an app on OAF

OAF installs and runs the AI platform on DGX Spark boxes: vLLM models, an OpenAI-compatible
gateway (agentgateway), semantic routing, Langfuse traces, Prometheus and Grafana. Your app does
not install or run any of that. It talks to OAF through two surfaces and nothing else.

Full references (read them before writing integration code, do not guess):
- Management API reference: `docs/api.md` in the OAF repository, or https://openagentfabric.dev/developers/api/
- Machine-readable spec: `docs/openapi.yaml`, or https://openagentfabric.dev/openapi.yaml
- Install and operations: `docs/user-guide.md`

## The two surfaces

| Surface | Used for | Credential | Where it runs |
|---|---|---|---|
| **Gateway** (OpenAI-compatible, `AGENT_GATEWAY_URL`) | Inference: chat, embeddings. Every request is traced. | App key (`AGENT_GATEWAY_API_KEY`) | Every request your app makes |
| **Management API** (`oafd`, `OAF_API_URL`, `/api/v1`) | Models, providers, guardrails, routing, keys, nodes | Admin token (`OAF_API_TOKEN`) | Your app's **backend**, for admins only |

The management API is never on the request path. Do not call it per user request.

## Getting credentials (an OAF admin does this once, on the control node)

```sh
sudo oaf keys issue <app-name> --admin
```

It prints env lines. Put them in your app's secret store, never in git and never in a browser:

- `AGENT_GATEWAY_URL`, `AGENT_GATEWAY_API_KEY`: the gateway and the app's key.
- `AGENT_GATEWAY_ROTATE_URL`: the app can replace its own key by POSTing the current key here.
- `AGENT_GATEWAY_MODEL` (the `auto` model name), and when set, the context window and embedding model.
- `LANGFUSE_HOST`, `LANGFUSE_PUBLIC_KEY`, `LANGFUSE_SECRET_KEY`: to add your own trace steps or read traces.
- With `--admin` only: `OAF_API_URL`, `OAF_API_TOKEN`, `OAF_API_CERT_SHA256` (the management API).

Running it again returns the same key. `--models a,b` limits what the key may use.

## Calling a model

Any OpenAI SDK works. Set the base URL to `AGENT_GATEWAY_URL` and the API key to `AGENT_GATEWAY_API_KEY`.

- `model` is a deployment name from the cluster, or the name of a LoRA adapter served on one (list them with
  `GET /v1/models` on the gateway).
- `model: auto` lets OAF's router choose: simple requests go to the fast model, hard ones to the large model.
  Explicit model names are never re-routed. Use `auto` unless you need a specific model.
- A model that fails its health check is replaced by the cluster's fallback model for `auto` requests, so check which
  model answered rather than assuming (`oaf.served_by` in the trace).
- Label calls with the headers `X-OAF-User`, `X-OAF-Session` and `X-OAF-Step`; they only label traces and grant nothing.
  To put your own spans in the same trace, pass a W3C `traceparent` header on each gateway call.
- Keep prompts out of logs you control. OAF records prompt and response text in Langfuse only when the
  cluster has `captureContent: true`; ask the admin which way it is set.

## Changing the platform (management API)

Every write is **two steps**, and your code must follow both:

1. Send the change (for example `POST /api/v1/models`). The reply is a **plan**: a diff, warnings, errors and a `plan_id`. Nothing has changed yet.
2. Show the diff to a human (or apply your own policy), then `POST /api/v1/plans/{id}/apply`. A `409` means the cluster changed since the plan; request a new plan.

Follow progress with `GET /api/v1/plans/{id}` or the `GET /api/v1/events` stream.

Some changes need a licence accepted first (a model's or an engine image's terms). The plan call then fails with `needs_acceptance`; a person accepts through `POST /api/v1/licenses/accept`. Never accept licences on a user's behalf in code.

## Rules

1. **Do not call the upstreams directly.** No requests to vLLM, agentgateway's admin port, Langfuse's admin or Prometheus to change anything. Control goes through `/api/v1`; inference goes through the gateway. OAF reconciles `cluster.yaml` and will overwrite out-of-band changes.
2. **Do not edit `cluster.yaml` or run `oaf apply` from your app.** The API does this, with validation.
3. **Secrets stay server-side.** The management token and the app key never reach a browser or a mobile client. Put your own backend between users and OAF.
4. **Least privilege.** Give apps only the gateway key (`oaf keys issue <app>`). Only an admin-facing backend gets `--admin`.
5. **Rotate keys with the rotate URL**, not by hand, and expect the old key to stop working at once. Rotation is limited to once every 30 seconds per key; retry gateway calls that fail around a rotation.
6. **External providers are off by default** and each needs `allowEgress: true`. A provider without guardrails is valid but warned in every plan. For sensitive data (patient records, credentials) keep traffic on local models, or attach guardrail policies (`GET /api/v1/guardrails`) before routing anything to an external provider.
7. **Apply a plan once.** Applying a plan twice, or while another plan is applying, answers `409`. After a network error, read `GET /api/v1/plans/{id}` before deciding anything. Plans expire after an hour.
8. **Use only what the reference lists.** The API is v1 and the project is early. `docs/api.md` section 10 lists what is not implemented (for example revoking a key, and high-availability topology).

## Checklist for a new integration

- [ ] Admin ran `oaf keys issue <app> --admin`; values are in the secret store.
- [ ] App uses an OpenAI SDK against `AGENT_GATEWAY_URL`, model `auto` or a named deployment.
- [ ] Backend (not browser) holds `OAF_API_TOKEN`; TLS is verified (pin `OAF_API_CERT_SHA256` when the certificate is self-signed).
- [ ] Writes go plan then apply, with the diff shown to a human.
- [ ] Health: `GET /api/v1/status` for nodes and models; a readiness check in the app uses `GET /v1/models` on the gateway.
- [ ] Traces: requests appear in Langfuse; if you add steps, use the `LANGFUSE_*` keys.
