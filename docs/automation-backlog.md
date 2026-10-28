# Automation backlog: steps still done by hand

OAF is installed and grown by one command and the console. Every step that had to be done by hand
on a box while building and testing it is listed here, with what was done and the automatic step
that replaces it. An entry is closed only when that automation is merged and has run on a box.

Status: **open**, **in progress**, **done (commit)**.

## Cluster growth (adding boxes)

| # | Done by hand | Where | Automatic replacement | Status |
|---|---|---|---|---|
| G1 | Rail addresses and MTU 9000 written to `/etc/netplan/40-oaf-fabric.yaml` and `netplan apply` (user guide §2) | new pair, 2026-10-24 | Agent `ConfigureFabric` RPC: writes the netplan file for the rails in `fabric.links` (never a port carrying other addresses), applies it, waits for carrier; run by `oaf apply` and `oaf install` before projects | done (92f4d68, dd01da9): ran on .142/.212, took over the hand-written port 1 file and added port 0; second run unchanged; stray address refused; `oaf apply` from the control box wrote both boxes' rails (6ca4302, 2026-10-24) |
| G2 | Second cable found by putting temporary addresses on port 0 of both boxes and pinging | new pair, 2026-10-24 | Cabling probe at plan time (`POST /api/v1/topology/recommend` for pending boxes): temporary 198.18.0.0/24 addresses on free up ports, ping, the answering MAC names the peer port, addresses removed; one link per cable | done (92f4d68): found both cables of .142/.212 in ~5 s, nothing left behind. Still to see: through oafd on the control box |
| G3 | Link check by ping and jumbo ping (`ping -M do -s 8972`) | new pair, 2026-10-24 | `oaf apply` runs the fabric check after G1: ping, MTU, `ib_write_bw` per rail, results on `/events` and in the plan steps | done (92f4d68): ran on .142/.212, four rails 104–112 Gb/s, 189/195 Gb/s per cable; from the control box: 111.6 Gb/s per rail, 196 Gb/s per cable. Still to see: the `/events` lines in the console |
| G4 | Join token expired (2 h) during a slow first join; a new token was made | 2026-10-24 | Console shows expiry and offers "new command"; join retries keep the token while preflight fails (already: failed preflight does not use it up) | open |
| G5 | Join copied all bundle images; `docker load` dropped their digests, 47 GB unusable, pruned by hand (`docker image prune`) | .212, 2026-10-24 | Join skips copying when registries are reachable (done a0f6c26, 270d85f); offline boxes pull from a registry on the control node (PLAN M5): the registry exists (2026-10-25: `registry` component on the control node, TLS from OAF's CA, every node trusts it at apply; `oaf images push/pin` for local components such as the patched GLM build); still open: pushing the whole bundle there for offline joins | in progress |
| G6 | `systemctl daemon-reload` after the agent's unit file changed | .212, 2026-10-24 | `oaf upgrade` (see U1) reloads units it changes. Again on .142/.212 (2026-10-24): the agent unit gained `ReadWritePaths=-/etc/netplan` for G1, installed by hand with `daemon-reload` and restart | open |
| G7 | Join token passed on the command line (visible in the process list) | join, 2026-10-24 | The join command exports `OAF_JOIN_TOKEN` and runs `sudo --preserve-env=OAF_JOIN_TOKEN bash -s`; join.sh sends it to curl on stdin and to `oaf join` in the environment (`--token` still accepted) | in progress (92f4d68): `--preserve-env` checked on DGX OS; needs a real join with the new script |
| G8 | Add-model form could not offer a new pair (only pairs that already ran a model) | console, 2026-10-24 | Status lists spec.groups; the form offers every pair (af8e2ee, Garrison 5a8164f) | done |
| G9 | The pair has two cables, but fabric.links lists only port 1 (the boxes were planned before the cabling probe existed), so apply removed port 0's probe-test addresses | pair, 2026-10-24 | A plan that adds a cable to an existing pair (probe → new link with subnets), and the vLLM renderer giving NCCL every rail of every link between the pair, not only the first link | open |

## Models

| # | Done by hand | Where | Automatic replacement | Status |
|---|---|---|---|---|
| M1 | GLM-5.3-Flash weights downloaded with `hf download` (pinned revision) to `/var/lib/oaf/models/<id>` on one box | .142, 2026-10-24; again for Qwen3-Coder on .242 (2026-10-25): the console's add-model plan applied with an empty weights directory and vLLM crash-looped; Ornith 1.5 and Qwen3-Next downloaded by hand for the companion benchmark | Done (2026-10-25): choosing a model starts the download on each of its nodes at once (pinned revision, disk check, resume, SHA-256 per file); apply holds the model until its weights are complete and the agent starts it then; status shows the download. Still open: copy over the fabric to a pair's second box instead of downloading twice; offline bundles | in progress |
| M2 | Weights copied to the second box of the pair over a ConnectX-7 rail with `tar -c | nc` → `nc -l <rail IP> | tar -x` (netcat installed if missing), then file-size lists compared | pair, 2026-10-24 | Same as M1 (fan-out step, over the fabric, with checksums) | open |
| M3 | Engine image `vllm/vllm-openai:glm53-flash-arm64-cu130` pulled by hand on both boxes | pair, 2026-10-24 | `oaf apply` pulls each deployment's pinned image on its nodes before starting it (pre-pull with progress) | open |
| M4 | vSR encoder artifact must be placed in `<modelsPath>/vsr-encoder` before reasoning signals work | gateway node | Pipeline fetches bundle artifacts by pinned revision (PLAN M2) | done (installer fetches it on the gateway node; ran on a fresh box 2026-10-28) |
| M5 | Model catalog on the box (`/etc/oaf/catalog/models.yaml`) replaced by hand with the repo's version; it is stale again (GLM still says 512K, repo says 256K). Installed by hand again (2026-10-24) for the companion models (Qwen3-Coder, Ornith) | .242, 2026-10-24 | The catalog ships in the bundle and `oaf upgrade` installs it | open |
| M7 | Catalog updated on the box by hand again (GLM profile's doubled `vllm serve` removed) and `oaf apply` run by hand to roll the fix out. Again (2026-10-24): GLM profile forces `--kv-cache-dtype bfloat16` (the checkpoint asks for FP8 KV, which the published build cannot run) and adds `--reasoning-parser glm45`; catalog installed and `oaf apply` run by hand | .242, 2026-10-24 | `oaf upgrade` installs the bundle's catalog and reconciles; a render test that an engine whose image ENTRYPOINT is `vllm serve` gets no commandPrefix | open |
| M8 | The patched GLM-5.3-Flash engine (`glm53:v9`, `vllm-glm53`) exists only as a local image: saved by hand with `docker save` before a full reset, to be loaded and pushed again with `oaf images push` | pair, 2026-10-28 | Build it from a Dockerfile and patches kept in the repo, or publish it to a registry the installer pulls from, pinned by digest in `bundle/versions.yaml` | open |
| M6 | NVIDIA CUDA EULA acceptance for a new engine image recorded in `spec.licenseAcceptances` | pending | Console asks for acceptance when a plan needs an image with included terms, and records who and when | open |

## Upgrades and services

| # | Done by hand | Where | Automatic replacement | Status |
|---|---|---|---|---|
| R1 | Full reset of three test boxes before a from-scratch install test: legacy images, staging directories and old checkouts deleted by hand; desktop restored as the default target | 3 boxes, 2026-10-28 | `oaf reset` (and the app's own uninstall) remove everything they installed; leftovers were from hand deployments before the installers existed | done (050161c) |
| U1 | New `oaf` binary copied to every node with scp, installed, services restarted (`oaf-node`, `oaf-keys`, `oaf-api`) | all boxes, repeatedly (last: 92f4d68 on .142/.212) | `oaf upgrade`: the control node pushes the bundle's binary to every agent, restarts services in order, checks versions | open |
| U2 | `oaf-api.service` installed by hand on the control box | .242, 2026-10-24 | Installer (`spark-bootstrap.sh` has the step; the one-command installer must too) | open |
| U3 | `oaf observability enable/scrape/grafana-url` run by the Garrison installer, `oaf apply` by hand after OAF upgrades | .242 | Part of the app install profile (PLAN M6) and `oaf upgrade` | open |
| U4 | Garrison containers started with `docker compose up -d --build` by hand: its installer stopped at `oaf keys issue`, which re-renders the whole cluster and failed on a model waiting for its licence | .242, 2026-10-24 | Installer keeps the key already in .env (Garrison commit after 5e7c375); OAF: `oaf keys issue` for an existing key should not need every project to render (an unrelated deployment must not block it) | in progress |
| U5 | A failed apply left the change in cluster.yaml (the GLM deployment without its licence), blocking later applies | .242, 2026-10-24 | oafd renders a change before writing it (6167323); licences are asked for at plan time and accepted in the console | done |
| U6 | Applying a new single-box model waited behind a pair model still loading: apply converges projects one at a time, so `model-ornith` was not created until GLM (weights still downloading) was healthy | .242, 2026-10-28 | Converge independent projects in parallel, or create every project before waiting on any | open |

## Garrison (appliance console)

| # | Done by hand | Where | Automatic replacement | Status |
|---|---|---|---|---|
| A1 | Deployed by `git bundle` + scp + `deployments/dgx-spark/install.sh` | .242 | Single-command install from the signed bundle (with OAF) | open |
| A2 | Installer prints the admin password at the end of its log | .242 | Print it once to the terminal only, never to a log file | open |
| A3 | Old vclocal Grafana/Prometheus/Loki/Tempo containers removed when moving to OAF's stack | .242 | Done by the installer (it removes them; volumes kept) | done |
| A4 | Restarted the follower rank of GLM-5.3-Flash (`docker restart` on spark-a98a) so it joined the leader's new attempt | spark-a98a | Follower ranks run under a rank guard and restart when they lose the leader (`a2e4cf4`) | done |
| A5 | GLM-5.3-Flash kernel compile OOM-killed (one nvcc per core beside the loaded weights) | pair-1 | Catalog profile: `MAX_JOBS=4`, `/root/.cache/flashinfer` kept as a volume (`04bcbb5`); healthy 22:21 UTC | done |
