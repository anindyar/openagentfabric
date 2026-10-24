# ADR-0003: vSR integration for `model: auto`

- **Status:** Accepted (implements ADR-0002 §4, milestone M3)
- **Date:** 2026-10-24
- **Deciders:** owner (ADR-0002); this record holds the spike evidence

## Context

`model: auto` must pick the model per request (simple → small local model, complex → large one),
from the models actually installed, with an observe mode, falling back when a model is down. vSR
(vLLM Semantic Router, `vllm-sr:v0.4.0`, pinned) is the router. The spike ran the pinned image on a
DGX Spark with two real chat backends: qwen-flash (local) and GLM-5.3-Flash (another box).

## Findings

- **Shape of the image.** `/app/start-router.sh` runs `router -config=/app/config.yaml -port=50051
  -enable-api=true`. 50051 is an **Envoy external processor** (gRPC ext_proc); 8080 is a management
  API bound to 127.0.0.1; 9190 is metrics. It is not an OpenAI proxy: vSR's own launcher puts Envoy
  (`envoyproxy/envoy:v1.35.3`, its `cli/consts.py`) in front.
- **Config.** The router reads the canonical v0.3 file (`providers.models[].backend_refs`,
  `routing.signals`, `routing.decisions[]` with `rules` and `modelRefs`; JSON Schema in the image at
  `/app/cli/config_schema/router-config-v0.3.schema.json`).
- **Decision protocol.** vSR sets the request header `x-selected-model`, rewrites the body's `model`,
  and returns `x-vsr-selected-decision`, `x-vsr-selected-model`, `x-vsr-routing-latency-ms` on the
  response. Envoy routes on `x-selected-model`. A request naming a real model bypasses vSR (no
  decision header): only `auto` is routed.
- **Signals and models.** `keyword` and `context` (token count) need no model: vSR started instantly
  at **36 MiB**. The `complexity` signal embeds the prompt with a local encoder
  (`llm-semantic-router/Vela-1.0-Encoder-307M-Embedding`, 1.2 GB float32 safetensors, Apache-2.0)
  on CPU: **1.68–1.71 GiB** resident. The image downloads it on first start, verifies the files
  (`.router-model-revision.json`, revision `1e57cebf…`) and, with the files present and
  `HF_HUB_OFFLINE=1` under `--network none`, starts without network (`required_models_already_present`).
  The external-embedding-endpoint setting did not replace the local encoder at this version.
  `domain` signals need a further classifier: not pinned yet.
- **Routing proven (two real models):** simple prompts → qwen-flash (decision `default`); a design
  prompt (complexity), a "step by step" prompt (keyword) and a 3,000-character log (context) →
  GLM-5.3-Flash (decision `complex`). Routing added 35–66 ms. Streaming kept streaming
  (`response_body_mode: NONE`, decision headers still returned).
- **agentgateway** traced vSR's response headers through CEL `response.headers[...]`: Langfuse
  generations carried the serving model and `oaf.route = simple|complex`.
- **vSR's own fallback does not help here.** Its per-model `reliability` health settings did not stop
  it choosing a down model, and Envoy aggregate clusters fail over **without** applying the chosen
  cluster's upstream filters, so the failed-over request still named the down model (404 from vLLM).

## Decision

1. **Standalone vSR behind Envoy, behind agentgateway.** agentgateway keeps auth, keys, tracing and
   every explicit model name. Its `auto` backend points at Envoy (`<gateway>:8899`, the port
   reserved for vSR). Envoy runs vSR as ext_proc (`failure_mode_allow`: with vSR down, requests still
   flow) and routes to a health-checked cluster per model (`/health`, which vLLM serves without a
   key). agentgateway's own ext_proc support was not used: its simplified LLM mode chooses the
   backend before an external processor could change it.
2. **Fallback and model naming in Envoy Lua** (built into the pinned image):
   - a downstream filter after ext_proc keeps vSR's choice unless that model fails its health check
     (read from Envoy's loopback admin `/clusters`), then uses `routing.defaults.fallback`; with no
     choice (vSR down) it uses `defaults.fast`; it sets `x-oaf-served-by`;
   - an upstream filter on each model cluster rewrites the body's `model` to that cluster's served
     name (vSR's body rewrite is not visible to a downstream filter).
   Proven: target down → served by the fallback; vSR down → served by fast.
3. **Observe mode:** every decision's `modelRefs` is the fast model; vSR still evaluates and its
   decision is traced (`oaf.route`), so admins see what active mode would do.
4. **Signals in OAF's schema:** `keyword`, `context` (`minTokens`) and `reasoning` (vSR complexity,
   OAF's built-in hard/easy example prompts, `threshold`, default 0.1). `domain` validates but is
   skipped with a warning.
5. **Pinning:** Envoy is a bundle component (digest, Apache-2.0). The encoder is a new bundle
   `artifacts` entry at the revision the vSR image verifies, mounted read-only at its expected path
   with `HF_HUB_OFFLINE=1`; it is only mounted when a reasoning signal is active.
6. **Memory:** `vsr` 128 MiB and `envoy` 64 MiB join the always-on stack limits (Prometheus 640→512,
   76 MiB measured; agentgateway 256→192, 17 MiB measured; total stays 8160 MiB). With the encoder,
   vSR gets the artifact's 1792 MiB, charged to the gateway node in the model memory budget like a
   model, not to the stack.
7. **The router project exists only when a rule is active** (target enabled, entitlement granted, a
   supported signal). Without one, `auto` goes straight to `defaults.fast` as before.

## Consequences

- Two more containers on the gateway node when routing is on; one more port (8899).
- Reasoning routing costs ~1.75 GiB and a 1.2 GB artifact on the gateway node. OAF does not fetch
  bundle artifacts yet (the M2 model pipeline will); until then the user guide gives the pinned
  fetch command, validation warns, and a missing encoder degrades to "everything to fast".
- Medical → DDx routing waits for a pinned domain classifier.
