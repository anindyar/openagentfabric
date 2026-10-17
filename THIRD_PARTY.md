# Third-party components

This is the register of every component that OAF installs, runs, or links. Every OAF release
must keep it accurate. A component may enter `bundle/versions.yaml` only after it has a row here.
The reasoning, risks and obligations for each licence are in [`docs/licence-audit.md`](docs/licence-audit.md).

Licences were last verified on **2026-10-17** against each project's repository (GitHub licence
metadata plus the LICENSE file wherever the metadata said `NOASSERTION`).

**How OAF uses each component.** Every component falls into one of these categories:
- **image**: an unmodified upstream container image, pinned by digest, run under docker compose.
- **host**: a host package or binary that OAF invokes. OAF does not link to it.
- **lib**: a library linked into the OAF binaries or the UI bundle. Listed per package once the
  stack is approved (ADR-0001). At M0 only the expected ones are shown.

## Runtime stack (deployed on every install)

| Component | Upstream | Licence (SPDX) | Use | Status / notes |
|---|---|---|---|---|
| agentgateway | github.com/agentgateway/agentgateway | Apache-2.0 | image | OK. Linux Foundation project. Latest release v1.6.0 (2026-10-02). |
| vLLM Semantic Router (vSR) | github.com/vllm-project/semantic-router | Apache-2.0 | image | OK. Latest release v0.4.0 (2026-09-27). |
| vLLM | github.com/vllm-project/vllm | Apache-2.0 | image | OK for the vLLM code. The image also contains NVIDIA CUDA/cuDNN/NCCL binaries under NVIDIA's terms, so it goes through the **user-acceptance flow** (audit §3.7). Arm64 release images exist (`vllm/vllm-openai:v0.31.0`, multi-arch). |
| Langfuse (web + worker) | github.com/langfuse/langfuse | MIT, excluding `ee/` dirs (proprietary EE licence) | image | OK when run unmodified. **Never copy code from `ee/`, `web/src/ee/` or `worker/src/ee/`.** Copyright holder is now ClickHouse, Inc. |
| PostgreSQL | postgresql.org | PostgreSQL | image | OK (permissive). |
| ClickHouse | github.com/ClickHouse/ClickHouse | Apache-2.0 | image | OK. |
| Valkey | github.com/valkey-io/valkey | BSD-3-Clause | image | Approved replacement for Redis (Redis ≥ 8 is RSALv2/SSPLv1/AGPLv3). Langfuse officially supports Valkey ≥ 8. Audit §3.2. |
| SeaweedFS | github.com/seaweedfs/seaweedfs | Apache-2.0 | image | Approved S3 store for Langfuse, replacing the archived MinIO. Langfuse support level is "community". Audit §3.3. |
| Prometheus | github.com/prometheus/prometheus | Apache-2.0 | image | OK. |
| node_exporter | github.com/prometheus/node_exporter | Apache-2.0 | image | OK. |
| dcgm-exporter | github.com/NVIDIA/dcgm-exporter | Apache-2.0 (exporter) | image | Exporter is OK. The DCGM libraries in the image are under NVIDIA terms. Support on GB10 is unverified (spike in M1). |
| Grafana | github.com/grafana/grafana | AGPL-3.0 | image | OK **only** as an unmodified image, embedded through iframe or its HTTP API. Never vendor or patch it. See audit §3.4. |

## Host-level dependencies (installed or checked by preflight)

| Component | Licence | Use | Notes |
|---|---|---|---|
| Docker Engine (moby) | Apache-2.0 | host | Docker Engine CE only. Never Docker Desktop. |
| Docker Compose v2 plugin | Apache-2.0 | host | Pinned in the bundle. |
| NVIDIA Container Toolkit | Apache-2.0 | host | Usually preinstalled on DGX OS. |
| NVIDIA driver / CUDA / NCCL (system libnccl 2.30.x for `LD_PRELOAD`) | NVIDIA proprietary (driver, CUDA) / BSD-3 + Apache-2.0 (NCCL) | host | Comes from DGX OS. OAF does not redistribute it. |
| perftest (`ib_write_bw`) | GPL-2.0 OR BSD-2-Clause (OpenIB) | host | Invoked as a binary. Not linked. |
| rdma-core / iproute2 / ethtool | GPL-2.0 / BSD mix | host | Invoked as binaries. |
| rsync | GPL-3.0 | host | Invoked as a binary to fan out weights. Not linked. |

## OAF Go dependencies (linked into `oaf`)

`make licenses` enforces these in CI (allowlist: Apache-2.0, MIT, BSD-2/3, ISC, MPL-2.0).

| Module | Version | Licence | Purpose |
|---|---|---|---|
| Go standard library | 1.27 | BSD-3-Clause | everything, incl. `crypto/ed25519` |
| github.com/spf13/cobra | v1.10.2 | Apache-2.0 | CLI |
| github.com/spf13/pflag | v1.0.9 | BSD-3-Clause | CLI flags (via cobra) |
| github.com/inconshreveable/mousetrap | v1.1.0 | Apache-2.0 | via cobra (Windows only) |
| github.com/santhosh-tekuri/jsonschema/v6 | v6.0.3 | Apache-2.0 | schema validation of cluster.yaml and the catalog |
| go.yaml.in/yaml/v3 | v3.0.5 | MIT + Apache-2.0 | YAML decoding |
| golang.org/x/text | v0.43.0 | BSD-3-Clause | schema error messages (via jsonschema) |

Planned (added with the PR that first uses them): connectrpc/connect-go (Apache-2.0),
go-git/go-git (Apache-2.0),
google/go-containerregistry (Apache-2.0); UI: React, Vite, TanStack (MIT).

## Not used (recorded so nobody adds them by accident)

| Component | Licence | Reason |
|---|---|---|
| Langfuse `ee/` code | Langfuse EE licence | Prohibited by CLAUDE.md rule 3. |
| Redis ≥ 8 | RSALv2 / SSPLv1 / AGPLv3 | Valkey is used instead. |
| MinIO | AGPL-3.0, archived | Unmaintained. SeaweedFS is used instead. |
| MinIO AIStor Free | Proprietary (free licence) | Not open source, and redistribution in an offline bundle is not granted. |
| Grafana Loki | AGPL-3.0 | Out of scope for v1. Would be acceptable as an unmodified image if needed. |
