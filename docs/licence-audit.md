# Upstream licence audit (M0)

- **Date:** 2026-10-17
- **Scope:** every component that the full-stack install deploys (CLAUDE.md, "Install"), plus host tools and the content OAF distributes (images, model weights).
- **Method:** GitHub licence metadata for each upstream repo. Where the metadata said `NOASSERTION`, the LICENSE/COPYING file was read. Upstream docs were checked for supported dependency versions.
- **Register:** [`THIRD_PARTY.md`](../THIRD_PARTY.md)

> This is an engineering audit, not legal advice. Items marked **LEGAL** need review by counsel
> before M5, when OAF starts redistributing an offline bundle.

## 1. Summary

| # | Finding | Severity | Action |
|---|---|---|---|
| F1 | **The MinIO upstream repo is archived and unmaintained.** The vendor now offers "AIStor Free" under a proprietary licence. Langfuse needs an S3 store. | ~~Blocker~~ Resolved | **Approved 2026-10-17: SeaweedFS** (Apache-2.0) replaces MinIO (§3.3). |
| F2 | Redis ≥ 8 is RSALv2/SSPLv1/AGPLv3. Redis ≤ 7.2 is BSD-3 but end-of-life. | Resolved | **Approved 2026-10-17: Valkey ≥ 8** (BSD-3) replaces Redis (§3.2). |
| F3 | The vLLM and dcgm-exporter images contain NVIDIA CUDA, cuDNN, NCCL and DCGM binaries under NVIDIA licence terms. | Decided (§3.7) | User-acceptance flow. These images are never redistributed by OAF; the user accepts NVIDIA's terms and fetches them (§3.1, §3.7). |
| F4 | Model weights have their own licences. Some are gated or non-redistributable (Llama, Gemma and similar). | Decided (§3.7) | Weights not under an allowlisted licence go through the user-acceptance flow and are never bundled (§3.5, §3.7). |
| F5 | Grafana is AGPL-3.0. | Medium, contained | Unmodified image, iframe/HTTP API only. Never patch or vendor it. Already in CLAUDE.md (§3.4). |
| F6 | Langfuse is MIT outside `ee/`. The copyright holder is now ClickHouse, Inc. The published image contains EE code, which stays inert without a licence key. | Low | Run the unmodified image. Ship no licence key. Copy no `ee/` code. Watch for licence changes at every pin bump (§3.6). |
| F7 | agentgateway, vSR, vLLM, Prometheus, node_exporter, ClickHouse, PostgreSQL, Docker, Compose and the NVIDIA Container Toolkit are all permissive (Apache-2.0 / BSD / PostgreSQL). | None | NOTICE files must be shipped in the bundle (§4). |

## 2. Per-component licences

See the table in `THIRD_PARTY.md`. Verified values:

| Component | SPDX | Source of truth |
|---|---|---|
| agentgateway | Apache-2.0 | GitHub metadata |
| vllm-project/semantic-router | Apache-2.0 | GitHub metadata |
| vllm-project/vllm | Apache-2.0 | GitHub metadata |
| langfuse/langfuse | MIT + proprietary `ee/` | LICENSE file (metadata says NOASSERTION because of the split) |
| grafana/grafana | AGPL-3.0 | GitHub metadata |
| prometheus, node_exporter | Apache-2.0 | GitHub metadata |
| NVIDIA/dcgm-exporter | Apache-2.0 | GitHub metadata |
| ClickHouse | Apache-2.0 | GitHub metadata |
| PostgreSQL | PostgreSQL License | well known (metadata NOASSERTION) |
| redis/redis (≥ 8) | RSALv2 / SSPLv1 / AGPLv3 (tri) | LICENSE.txt |
| valkey-io/valkey | BSD-3-Clause | GitHub metadata |
| minio/minio | AGPL-3.0, **archived** | GitHub metadata + README ("no longer maintained") |
| seaweedfs/seaweedfs | Apache-2.0 | GitHub metadata |
| rustfs/rustfs | Apache-2.0 | GitHub metadata (alternative to SeaweedFS) |
| garage | AGPL-3.0 | GitHub metadata (rejected: AGPL with no advantage over SeaweedFS) |
| NVIDIA/nccl | Apache-2.0 + BSD parts | LICENSE.txt |
| linux-rdma/perftest | GPL-2.0 OR BSD-2 (OpenIB) | COPYING |
| moby, docker/compose, nvidia-container-toolkit | Apache-2.0 | GitHub metadata |

## 3. Findings in detail

### 3.1 NVIDIA binaries inside images (F3)
Upstream builds `vllm/vllm-openai` on NVIDIA CUDA base images. Those images contain CUDA runtime
libraries, cuBLAS, cuDNN, NCCL and other components. NVIDIA's CUDA EULA lets developers redistribute
certain listed runtime components *as part of an application*. NGC containers and the base images
carry their own licence terms.

- **Online install (M1–M4).** Each node pulls the image from the upstream registry by digest, so OAF
  does not redistribute anything. No issue.
- **Offline bundle (M5).** Here OAF *does* redistribute the image as an OCI archive. **LEGAL:**
  confirm that redistributing an unmodified `vllm/vllm-openai` image (and the dcgm-exporter
  image) inside a customer bundle is allowed. If it is not allowed, there are two fallbacks:
  1. Ship the bundle without the images. The customer runs `oaf bundle fetch` on a connected
     machine, which pulls the images under their own acceptance of NVIDIA's terms.
  2. Use the NVIDIA-published vLLM container from NGC under the NGC terms, if those terms permit
     redistribution.

### 3.2 Redis → Valkey (F2)
Langfuse v4 docs: Redis ≥ 7 and Valkey ≥ 8 are both "Official". The instance must have
`maxmemory-policy=noeviction`. Valkey is BSD-3, avoids the Redis 8 licence question, and needs no
code change in Langfuse because it is configured through `REDIS_HOST`. **Proposed: Valkey 8.x,
pinned by digest.**

### 3.3 MinIO is archived (F1). Decided: SeaweedFS
The upstream repo `minio/minio` is archived. The README says it is "no longer maintained" and points
to AIStor Free (a proprietary free licence) or AIStor Enterprise. The Langfuse docs still list
"MinIO / AIStor" as their officially supported self-hosted S3 store, and their reference compose
file uses MinIO.

| Option | Licence | Maintained | Langfuse support | Offline redistribution | Verdict |
|---|---|---|---|---|---|
| Pin the last MinIO AGPL image | AGPL-3.0 | No (no CVE fixes) | Official | Allowed | Reject: ships an unmaintained network service |
| AIStor Free | Proprietary | Yes | Official | Not granted | Reject: not open source, and the bundle cannot include it |
| **SeaweedFS** | Apache-2.0 | Yes, mature | Community | Allowed | **Recommended** |
| RustFS | Apache-2.0 | Yes, younger | Community | Allowed | Fallback |
| Garage | AGPL-3.0 | Yes | Community | Allowed | Reject: no advantage, adds AGPL |

Langfuse uses S3 for three things: raw event upload (mandatory), media and batch export. All of
them use path-style S3 with configurable endpoints. The acceptance test already checks that traces
are present, so it will catch an incompatibility.

**Decision (owner, 2026-10-17):** adopt SeaweedFS in single-node S3 mode. A Langfuse-on-SeaweedFS
check is part of the M4 exit criteria, and RustFS is the documented fallback. CLAUDE.md is updated.

### 3.4 Grafana (AGPL-3.0) (F5)
OAF runs Grafana unmodified and talks to it only through the `allow_embedding` iframe, the HTTP
API (dashboards, datasources) and provisioning files. Under this model OAF is not a derivative work. The
AGPL source-offer duty for network use attaches to modifications of Grafana, and OAF makes none.
Rules:
- No Grafana source code and no Grafana frontend packages in `ui/`. `@grafana/*` npm packages are
  mostly Apache-2.0, but **do not import any of them without a per-package check**, because some are AGPL.
- The dashboards OAF provisions are OAF's own JSON, licensed Apache-2.0.
- The bundle includes a pointer to the Grafana source for the pinned version (good practice).

### 3.5 Model weights (F4)
Weights are content, not software, but OAF downloads them, rsyncs them and (in M5) may bundle them.
Licences vary: Apache-2.0 (many Qwen and Mistral models), custom community licences (Llama), Gemma
Terms, and NVIDIA Open Model License. Some require accepting terms on Hugging Face (gated).
- Every catalog entry must declare `license.spdx` (or `license.name` + `url`), `gated` and
  `redistributable`. These fields are required in `schema/catalog.schema.json`.
- **Online install:** each customer pulls the weights with their own HF token, so they accept the
  terms themselves.
- **Offline bundle:** `oaf bundle build` refuses any model with `redistributable: false`.
- **DDx** is OAF-entitled content. Its weights are delivered separately and unlocked by the
  entitlement file, never by the public bundle.

### 3.6 Langfuse specifics (F6)
- The Docker images `langfuse/langfuse` and `langfuse/langfuse-worker` are built from the same
  codebase as the commercial edition. EE features (such as the instance management API, org-creator
  restrictions and UI customisation) are inert without `LANGFUSE_EE_LICENSE_KEY`. OAF must not set
  that key and must not depend on any EE feature.
- OAF uses only the public HTTP API (traces, metrics) and OTEL ingestion. Both are MIT-side surface.
- At every Langfuse pin bump, CI diffs the upstream `LICENSE` against the recorded hash and fails
  if it changed.

### 3.7 Decision: user-acceptance flow (owner, 2026-10-17)
Where a licence is not clearly permissive, OAF does not accept or redistribute on the user's behalf.
It sends the user to the upstream licence or download page, and the user accepts it there.

- **Allowlist** (no acceptance needed): Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause, ISC,
  PostgreSQL, and GPL-2.0/GPL-3.0/AGPL-3.0 (copyleft artefacts, such as the Grafana image, may be bundled only
  with the corresponding-source offer included, see §4).
- **Everything else needs acceptance:** gated models, custom model licences (Llama, Gemma, NVIDIA
  Open Model License, …), images containing NVIDIA CUDA/DCGM binaries, and any entry with no SPDX id.
- The catalog (and later `bundle/versions.yaml`) records an `acceptUrl` for each such artefact.
  Schema and semantic validation make it mandatory.
- **The acceptance is recorded in `cluster.yaml`** (`spec.licenseAcceptances`: subject, URL, who, when),
  per CLAUDE.md rule 4. It is never UI-only state. The UI flow is: show the licence name → open
  `acceptUrl` → user confirms → `oafd` commits the acceptance → fetch/deploy is unblocked.
- OAF refuses to pull or deploy an artefact that needs acceptance until a matching entry exists.
- **Offline bundle:** contains only allowlisted artefacts. Artefacts that need acceptance are fetched
  by the user with `oaf bundle fetch` on a connected machine, after accepting.

This removes the need for OAF to redistribute anything under NVIDIA or custom model terms. Counsel
review is still advisable before M5 for the wording of the acceptance screen and the GPL source offer.

## 4. Obligations OAF must implement

1. **Apache-2.0 NOTICE files.** The bundle includes `licenses/<component>/{LICENSE,NOTICE}`,
   extracted from each pinned image or repo. Build step: `oaf bundle licenses`.
2. **THIRD_PARTY.md must stay current.** CI fails when `bundle/versions.yaml` names a component
   that has no row in THIRD_PARTY.md.
3. **Licence drift check.** `bundle/versions.yaml` records the SHA-256 of each upstream's licence
   file at the pinned tag. `make license-drift` (run in CI) re-fetches and compares them, and fails on
   any change. A human then re-audits. Implemented in `tools/licensedrift`.
4. **No vendored AGPL code** (Grafana, MinIO, Garage) and **no `ee/` code**. A CI grep guard runs
   on `ui/` and `go.mod` / `package.json`.
5. **Go and npm dependency licences.** `go-licenses` / `license-checker` run in CI, with an allowlist of
   Apache-2.0, MIT, BSD-2/3, ISC, MPL-2.0 (file-level) and PostgreSQL. Anything else fails CI.

## 5. Open items for the owner
- [x] Approve **Valkey** in place of Redis.
- [x] Approve **SeaweedFS** in place of MinIO (or choose RustFS / another option).
- [x] Arrange **LEGAL** review of NVIDIA image redistribution and model-weight bundling before M5. (in our ui we can redirect the user to the correct download page and let the accept the license whereever we have doubt (non gpl, no apache, no mit license)
  - → Recorded as §3.7. Optional: counsel review of the acceptance-screen wording before M5.
