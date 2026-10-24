# Automation backlog: steps still done by hand

OAF is installed and grown by one command and the console. Every step that had to be done by hand
on a box while building and testing it is listed here, with what was done and the automatic step
that replaces it. An entry is closed only when that automation is merged and has run on a box.

Status: **open**, **in progress**, **done (commit)**.

## Cluster growth (adding boxes)

| # | Done by hand | Where | Automatic replacement | Status |
|---|---|---|---|---|
| G1 | Rail addresses and MTU 9000 written to `/etc/netplan/40-oaf-fabric.yaml` and `netplan apply` (user guide §2) | new pair, 2026-10-24 | Agent `ConfigureFabric` RPC: writes the netplan file for the rails in `fabric.links` (never a port carrying other addresses), applies it, waits for carrier; run by `oaf apply` before projects | open |
| G2 | Second cable found by putting temporary addresses on port 0 of both boxes and pinging | new pair, 2026-10-24 | Cabling discovery at join/plan time: probe each up port with temporary link-local or /30 addresses (removed after), record which port reaches which box, and plan one link per cable | open |
| G3 | Link check by ping and jumbo ping (`ping -M do -s 8972`) | new pair, 2026-10-24 | `oaf apply` runs the fabric check after G1: ping, MTU, `ib_write_bw` per rail, results on `/events` and in the plan steps | open |
| G4 | Join token expired (2 h) during a slow first join; a new token was made | 2026-10-24 | Console shows expiry and offers "new command"; join retries keep the token while preflight fails (already: failed preflight does not use it up) | open |
| G5 | Join copied all bundle images; `docker load` dropped their digests, 47 GB unusable, pruned by hand (`docker image prune`) | .212, 2026-10-24 | Join skips copying when registries are reachable (done a0f6c26, 270d85f); offline boxes pull from a registry on the control node (PLAN M5) | in progress |
| G6 | `systemctl daemon-reload` after the agent's unit file changed | .212, 2026-10-24 | `oaf upgrade` (see U1) reloads units it changes | open |
| G7 | Join token passed on the command line (visible in the process list) | join, 2026-10-24 | Pass it to `oaf join` through the environment or stdin | open |
| G8 | Add-model form could not offer a new pair (only pairs that already ran a model) | console, 2026-10-24 | Status lists spec.groups; the form offers every pair (af8e2ee, Garrison 5a8164f) | done |

## Models

| # | Done by hand | Where | Automatic replacement | Status |
|---|---|---|---|---|
| M1 | GLM-5.3-Flash weights downloaded with `hf download` (pinned revision) to `/var/lib/oaf/models/<id>` on one box | .142, 2026-10-24 | Model pipeline (PLAN M2): download on one node, check disk first, fan out over the fabric to every node of the group, verify checksums | open |
| M2 | Weights copied to the second box of the pair over a ConnectX-7 rail with `tar -c | nc` → `nc -l <rail IP> | tar -x` (netcat installed if missing), then file-size lists compared | pair, 2026-10-24 | Same as M1 (fan-out step, over the fabric, with checksums) | open |
| M3 | Engine image `vllm/vllm-openai:glm53-flash-arm64-cu130` pulled by hand on both boxes | pair, 2026-10-24 | `oaf apply` pulls each deployment's pinned image on its nodes before starting it (pre-pull with progress) | open |
| M4 | vSR encoder artifact must be placed in `<modelsPath>/vsr-encoder` before reasoning signals work | gateway node | Pipeline fetches bundle artifacts by pinned revision (PLAN M2) | open |
| M5 | Model catalog on the box (`/etc/oaf/catalog/models.yaml`) replaced by hand with the repo's version; it is stale again (GLM still says 512K, repo says 256K) | .242, 2026-10-24 | The catalog ships in the bundle and `oaf upgrade` installs it | open |
| M6 | NVIDIA CUDA EULA acceptance for a new engine image recorded in `spec.licenseAcceptances` | pending | Console asks for acceptance when a plan needs an image with included terms, and records who and when | open |

## Upgrades and services

| # | Done by hand | Where | Automatic replacement | Status |
|---|---|---|---|---|
| U1 | New `oaf` binary copied to every node with scp, installed, services restarted (`oaf-node`, `oaf-keys`, `oaf-api`) | all boxes, repeatedly | `oaf upgrade`: the control node pushes the bundle's binary to every agent, restarts services in order, checks versions | open |
| U2 | `oaf-api.service` installed by hand on the control box | .242, 2026-10-24 | Installer (`spark-bootstrap.sh` has the step; the one-command installer must too) | open |
| U3 | `oaf observability enable/scrape/grafana-url` run by the Garrison installer, `oaf apply` by hand after OAF upgrades | .242 | Part of the app install profile (PLAN M6) and `oaf upgrade` | open |

## Garrison (appliance console)

| # | Done by hand | Where | Automatic replacement | Status |
|---|---|---|---|---|
| A1 | Deployed by `git bundle` + scp + `deployments/dgx-spark/install.sh` | .242 | Single-command install from the signed bundle (with OAF) | open |
| A2 | Installer prints the admin password at the end of its log | .242 | Print it once to the terminal only, never to a log file | open |
| A3 | Old vclocal Grafana/Prometheus/Loki/Tempo containers removed when moving to OAF's stack | .242 | Done by the installer (it removes them; volumes kept) | done |
