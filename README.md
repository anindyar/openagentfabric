# openagentfabric (OAF)

A control plane and UI for running an agent/LLM platform on a small GB10 cluster
(NVIDIA DGX Spark / Asus GX10). Installing OAF installs the whole stack: agentgateway, the vLLM
Semantic Router, vLLM, Langfuse, Prometheus and Grafana, all pinned and unmodified.

Status: early development (milestone M1: CLI install of the stack on one node or a pair).

## Documentation

- [User guide](docs/user-guide.md): requirements, step-by-step install, what to expect, operations, troubleshooting.
- One box, one command: `scripts/spark-bootstrap.sh --email you@example.com` on a new DGX Spark (user guide §4).
- [Architecture](docs/architecture.md): how OAF works, and why it builds on pairs of nodes rather than a 4-node cluster.
- [Plan](docs/PLAN.md): milestones, schemas, validation rules and progress.
- [ADR-0001](docs/adr/0001-stack.md): implementation stack. [Licence audit](docs/licence-audit.md).

## Build

```sh
make test          # unit tests
make build         # bin/oaf for linux/arm64 (the target platform)
make build-native  # bin/oaf for the host platform
```

Licence: Apache-2.0. Third-party components: [THIRD_PARTY.md](THIRD_PARTY.md).
