# Installing OAF (M1, CLI only)

M5 turns this into one `curl … | bash`. Until then the node agents are put in place by hand, and
`oaf install` does the rest. Run everything on the **control node** (the node with the `control`
role) unless a step says otherwise.

## 1. Build or fetch the binary

```sh
make build            # bin/oaf-linux-arm64 (static)
sudo install -m 0755 bin/oaf-linux-arm64 /usr/local/bin/oaf
```

## 2. Describe the cluster

Copy `examples/cluster.pair.yaml` to `/etc/oaf/cluster.yaml` and `examples/catalog.yaml` to
`/etc/oaf/catalog/models.yaml`, then edit the node names and management addresses. Check it:

```sh
oaf validate
oaf preflight --ssh user@<each node>    # read-only; changes nothing
```

Some images ship terms that must be accepted before OAF deploys them. vLLM includes NVIDIA's CUDA
libraries, so read <https://docs.nvidia.com/cuda/eula/index.html> and record it under
`spec.licenseAcceptances` (subject `image/vllm`). The example file shows the format.

## 3. Certificates

```sh
sudo oaf pki init --dir /etc/oaf/pki --cluster <name> \
  --node spark-1=192.168.1.11 --node spark-2=192.168.1.12
```

This writes the cluster CA, the `oafd` client certificate (used by `oaf apply/status/…`) and
`nodes/<name>/{ca.crt,node.crt,node.key}` for each node. Keep `ca.key` on the control node only.

## 4. Node agent on every node

On each node (including the control node), with that node's files from step 3:

```sh
sudo install -m 0755 oaf /usr/local/bin/oaf
sudo install -d -m 0700 /etc/oaf/pki
sudo install -m 0644 ca.crt node.crt /etc/oaf/pki/
sudo install -m 0600 node.key /etc/oaf/pki/
sudo install -m 0644 deploy/systemd/oaf-node.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now oaf-node
```

The agent listens on port 7701 and accepts only the `oafd` certificate of this cluster. It manages
only compose projects named `oaf-*` under `/var/lib/oaf/projects`, so it never touches other
containers.

## 5. Check the fabric

Each link in `fabric.links` must already have an address on every rail (both ends in one subnet, MTU 9000);
OAF does not assign them yet. Check the links and measure them (each rail alone, then both together):

```sh
oaf fabric check                     # through the agents
oaf fabric check --ssh spark-1=user@192.168.1.11 --ssh spark-2=user@192.168.1.12   # before the agents
```

The run loads each link for a few seconds. `oaf install` runs it too.

## 6. Install

```sh
sudo oaf install           # preflight and fabric check on every node, then deploy everything
oaf status                 # per-project health; models can take many minutes to load
```

`oaf install` stops if any node fails preflight or any link fails the fabric check. Afterwards, change `cluster.yaml` and run
`oaf apply --dry-run` to see the plan, then `oaf apply`. Re-running with no changes only converges.
Secrets are generated on the first apply into `/etc/oaf/secrets` (0600) and reach containers only
through each project's `.env`.

Model weights are expected under `/var/lib/oaf/models/<catalog id>` on every node of a deployment.
The download and fan-out pipeline arrives in M2; until then, place them by hand.

## Uninstall

```sh
sudo oaf uninstall --yes            # removes every oaf-* project; keeps volumes
sudo oaf uninstall --yes --purge    # also deletes Langfuse data, metrics and dashboards
```

Weights, secrets, certificates and the agents stay. To remove the agent:
`sudo systemctl disable --now oaf-node && sudo rm -r /etc/systemd/system/oaf-node.service /var/lib/oaf/projects`.
