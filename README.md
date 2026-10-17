# openagentfabric (OAF)

A control plane and UI for running an agent/LLM platform on a small GB10 cluster
(NVIDIA DGX Spark / Asus GX10). Installing OAF installs the whole stack: agentgateway, the vLLM
Semantic Router, vLLM, Langfuse, Prometheus and Grafana, all pinned and unmodified.

Status: early development (milestone M1). See [docs/PLAN.md](docs/PLAN.md).

## Build

```sh
make test          # unit tests
make build         # bin/oaf for linux/arm64 (the target platform)
make build-native  # bin/oaf for the host platform
```

Licence: Apache-2.0. Third-party components: [THIRD_PARTY.md](THIRD_PARTY.md).
