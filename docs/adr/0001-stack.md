# ADR-0001: Implementation stack for OAF

- **Status:** Accepted (owner, 2026-10-17)
- **Date:** 2026-10-17
- **Deciders:** owner

## Context

OAF has four code surfaces:

1. **`oafd`**: control plane. It serves the API and the UI, runs the reconciler (desired `cluster.yaml` →
   actual state across nodes), renders upstream configs (compose, agentgateway, vSR, Prometheus,
   Grafana provisioning) and verifies entitlements.
2. **`oaf-node`**: the agent on each node. It runs as root under systemd. It drives `docker compose`,
   checks NICs and RDMA (`ip`, `ethtool`, `ibv_devinfo`, `ib_write_bw`), runs disk checks, does
   rsync fan-out and HF downloads, and reports health.
3. **`oaf` CLI**: `install`, `preflight`, `apply`, `status`, `bundle`, `uninstall`.
4. **`ui/`**: the web UI. It embeds Grafana through iframes and uses the Langfuse API.

These constraints come from CLAUDE.md and the hardware:
- arm64 first (GB10). It must work **air-gapped** from a signed offline bundle.
- Nodes are managed boxes. The fewer runtime dependencies on the host, the better.
- Everything must be idempotent. One declarative source of truth. Images pinned by digest.
- The agentgateway fork, if we ever need one, must use the fork's own languages: Rust for the core and
  admin API, TypeScript/React for the UI.
- The upstreams OAF integrates with are written in Rust (agentgateway), Go (vSR, Prometheus,
  Grafana backend, Docker/Compose), Python (vLLM) and TypeScript (Langfuse, the agentgateway UI).

## Decision

### Backend: **Go** for `oafd`, `oaf-node` and `oaf` (a single multi-call binary)

- Produces **one static, CGO-free arm64 binary** with trivial cross-compilation. Install and offline
  bundling become "copy one file". There is no interpreter or venv on the host, so there is nothing
  to break when DGX OS updates Python.
- It is the native ecosystem for what OAF orchestrates: the Docker/Compose libraries and types, the
  Prometheus client, and vSR's own Go config types. We can import vSR's config structs for typed
  rendering, or at least validate against them in tests.
- The standard library covers our security primitives: `crypto/ed25519` for entitlements and
  bundle signatures, and `crypto/tls` for mTLS between nodes.
- Its strength is long-running supervisor daemons, which is what both `oafd` and `oaf-node` are.

Rejected alternatives:
- **Rust** matches the agentgateway core. But iteration is slower, the docker/compose ecosystem is
  weaker, and the agentgateway code we might touch in a fork is UI or admin-API code that would be in Rust
  regardless. Using Rust in OAF does not help the fork.
- **Python** matches vLLM. But it adds a runtime and dependency closure to manage on air-gapped arm64
  hosts, and packaging a root daemon is fragile.
- **TypeScript/Node** would share a language with the UI. But it is a poor fit for a root node agent
  that shells out to RDMA tooling, and it needs a runtime on every host.

### UI: **TypeScript + React + Vite + TanStack Router/Query + Biome**

This is the stack the agentgateway UI uses (`agentgateway/ui/package.json`). Matching it means:
- Any agentgateway UI or admin extensions (CLAUDE.md rule 2) can be built in the fork and pulled
  into the OAF UI, or contributed upstream, without changing frameworks.
- OAF can read agentgateway's published `schema/config.json` and `schema/admin.json` to generate
  types, as the agentgateway UI already does.

The built UI is embedded into the `oafd` binary with `go:embed`, so there is no separate web server
image. Grafana panels use `<iframe>` (Grafana `allow_embedding = true`, with auth through `oafd`
acting as an auth proxy). Langfuse uses its public HTTP API, plus deep links into the Langfuse UI
for detail views. Whether Langfuse can be iframed is an M4 spike.

### APIs: **Connect RPC (protobuf)** for UI↔oafd and oafd↔oaf-node

- Uses `connectrpc.com/connect` (Apache-2.0). One `.proto` definition gives gRPC between daemons and
  plain HTTP/JSON for the browser, without a gRPC-web proxy.
- Uses `buf` for linting and breaking-change detection, which agentgateway already uses.
- oafd↔oaf-node traffic uses **mTLS**. The installer creates a cluster CA and issues node
  certificates on each install. No secrets are committed.

### State: **files, with git history**

- Desired state is `/etc/oaf/cluster.yaml` plus `catalog/models.yaml`, validated against
  `schema/*.schema.json`. The directory is a local git repo (`go-git`). Every UI or CLI edit becomes
  a commit, which gives audit history and `oaf apply --rollback`. The UI never holds state of its
  own (CLAUDE.md rule 4). It edits the file through `oafd`, which validates and commits.
- **Observed state** (health, tok/s, link status) is not desired state. It lives in memory in
  `oafd`, is rebuilt from node reports, and time series go to Prometheus. There is no database in v1.
- **Secrets** are generated at install, stored in `/etc/oaf/secrets/` (mode 0600) and
  referenced by name from `cluster.yaml` (`secretRef`). They are never written into the YAML or the
  git history.

### Rendering and runtime

- Upstream configs and compose files are rendered with Go `text/template` from typed structs.
  **Golden-file tests** cover every renderer.
- `oaf-node` shells out to the **`docker compose` plugin, pinned in the bundle**. It does not use
  compose as a Go library, because the CLI is the stable contract. Each component gets its own
  compose project (`oaf-<component>`), so components upgrade independently and idempotently.
- systemd units: `oaf-node.service` on every node, and `oafd.service` on the control node.

### Packaging and supply chain

- `install.sh` is a short POSIX `sh` bootstrap. It downloads the `oaf` binary and the bundle
  manifest, verifies the **ed25519 signature** against the release key embedded in the script, and
  then runs `oaf install`.
- Offline bundle: a signed manifest, plus image OCI archives saved with `go-containerregistry`, plus
  the `oaf` binary, licences and (only redistributable) weights.
- Builds are arm64 first, then multi-arch. GoReleaser (MIT) is a build tool and is not shipped.

### Testing

- Go: `go test` covering unit tests, golden render tests, schema tests (examples must validate) and
  semantic-validation tests.
- UI: `node --test` and Playwright, matching agentgateway.
- `test/e2e`: the acceptance test as `oaf acceptance`. It runs on real hardware after every install
  (per-model health and tok/s against the catalog baseline, rail bandwidth, 3 routing prompts,
  Langfuse traces).

## Consequences

- One language each for the backend and the frontend. The agentgateway fork, if it ever happens,
  is the only Rust.
- Go cannot share types with the UI directly. Protobuf and the generated TS clients (`buf generate`) cover this.
- Choosing to shell out to `docker compose` instead of using a library means parsing CLI output.
  We use `docker compose ... --format json` everywhere.
- No database means observed history beyond Prometheus retention is lost. That is acceptable for v1.

## Open points for review
1. **Single control node in v1.** If the control node dies, `oafd` is offline, but inference and
   routing keep running because they are configured statically. Peers keep a git mirror of
   `/etc/oaf` so another node can be promoted manually (`oaf promote`). OK for v1?
2. Is the Connect RPC choice fine, or do you prefer plain REST + OpenAPI?
