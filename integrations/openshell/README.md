# NVIDIA OpenShell on Agent Substrate (ate-env)

Runs [OpenShell](https://github.com/NVIDIA/OpenShell) sandboxes as ate-env
environments: the OpenShell gateway keeps its API, policy model and CLI, and a
compute driver in this directory creates one Substrate actor per sandbox. The
two trusted OpenShell binaries, `openshell-sandbox` and `openshell-supervisor`,
run inside the actor from a runtime layer, next to the ate-env guest.

Status: M0 spike of [the plan](../../../substrate-explorations/docs/plans/openshell-runtime-for-ate-env.md)
(kept outside this repo). Verified 2026-10-16 on a GKE cluster running
Substrate `v0.4.0-83-g960997ab` with the `microvm` sandbox class.

| step | result |
|---|---|
| `openshell-sandbox capability-probe-launch 65534 65534` in a microVM actor | `qualified: true`, Landlock ABI 7, seccomp notification with ADDFD, DNS relay |
| the same on a gVisor actor | fails after the privilege drop (`read /proc/self/status: EACCES`); gVisor has no Landlock either |
| `openshell sandbox create --from debian@sha256:… -- sleep 3600` | Ready in about 15 s (create returns after 12 s) |
| `openshell sandbox exec t3 -- id` | `uid=65534(nobody)` on kernel 6.18.35 in the unmodified image, cwd `/sandbox` |
| `openshell sandbox stop` | actor SUSPENDED in 10 s; the gateway then reports Error and refuses `start` (open, see below) |
| `openshell sandbox delete` | environment deleted |

## Pieces

| path | what |
|---|---|
| `layer/` | `Dockerfile` for the runtime layer: `openshell-sandbox` (static musl), `openshell-supervisor` with a bundled glibc and loader (`bin/supervisor` wrapper), static busybox. linux/amd64. |
| `../../cmd/openshell-driver-substrate`, `../../internal/openshelldriver` | the compute driver: `openshell.compute.v1.ComputeDriver` over a Unix socket, built on `clients/go`. `bootstrap.go` is the OpenShell v0.1.4 launch contract; `launch.go` stages it in the actor and starts the binaries. |
| `deploy/gateway.toml`, `deploy/gateway.yaml` | a plaintext, unauthenticated gateway for a test cluster, with the driver as a sidecar sharing the socket volume. |
| `hack/egress-policies.sh` | gives every `os-*` actor an EgressPolicy to the gateway. Stand-in until the driver creates the policy itself. |

## How a sandbox is created

1. The gateway calls the driver's `CreateSandbox` with the image, the command,
   the policy and a signed launch bundle.
2. The driver creates an ate-env environment `os-<hash of the sandbox id>` from
   the base template with `rootfs_image` set to the sandbox image; ate-env-api
   derives a template per image and reuses it.
3. Through the guest, the driver lowers `net.ipv4.ip_unprivileged_port_start`
   to 0 (the sandbox's DNS relay binds 127.0.0.53:53 without capabilities),
   creates `/.openshell/channel/sandbox`, `/.openshell/supervisor`,
   `/run/openshell`, `/run/openshell-supervisor-ca/material` and `/sandbox`,
   and writes the boundary bootstrap, the per-session TLS material, the
   runtime descriptor and the supervisor auth bundle.
4. It starts `openshell-sandbox launch-capability-free <uid> <gid> <bootstrap> /sandbox`
   (drops every capability, switches to the workload identity, sets
   no_new_privs) and then `openshell-supervisor` with `OPENSHELL_ENDPOINT`
   pointing at the gateway service, by URL with scheme.
5. The supervisor dials out to the gateway through atenet-egress; the actor's
   EgressPolicy must allow cleartext HTTP to the gateway host and port. The
   gateway marks the sandbox Ready when the supervisor session is up.

## Preconditions on the cluster

- A worker pool of the `microvm` class whose worker image was built from the
  cluster's own Substrate commit (an older `ateom-microvm` fails its capacity
  report with `default_runtime.version: Required value`), with
  `resources.limits` on the pool template.
- A base ActorTemplate with the layer at `/opt/openshell`, class
  `SANDBOX_CLASS_MICROVM`, `resources.limits` (cpu, memory) and
  `securityContext.capabilities.add: [SETPCAP, SETUID, SETGID, CHOWN,
  DAC_OVERRIDE, FOWNER]`. Actors run as root with only KILL,
  NET_BIND_SERVICE and AUDIT_WRITE by default, which is not enough for
  OpenShell's launcher. `ate-env manifest template --layer openshell=<layer>@sha256:…=/opt/openshell`
  produces the rest; edit the class, config name, resources and capabilities in
  until the manifest command grows flags for them.
- The `openshell` namespace must be reachable from atenet-egress (it is, by
  default) and each OpenShell actor needs an EgressPolicy
  (`hack/egress-policies.sh`).

## Test drive

```bash
kubectl -n openshell port-forward svc/openshell-gateway 18080:8080 &
openshell gateway add --name substrate http://127.0.0.1:18080 && openshell gateway select substrate
CONTEXT=<kube context> integrations/openshell/hack/egress-policies.sh &
openshell sandbox create --name t1 --from docker.io/library/debian@sha256:<digest> --detach --no-tty --no-auto-providers -- sleep 3600
openshell sandbox exec t1 -- id
openshell sandbox delete t1
```

Images must be pinned by digest: ate-env derives a template per image and does
not resolve tags.

## Known gaps (M1 and later)

- The driver does not create the actor's EgressPolicy; the helper script does,
  and it can lose the race against the supervisor's first connection attempt
  (the supervisor retries, so this only slows the first start).
- Driver records live in memory; a driver restart forgets its sandboxes.
- `stop` suspends the actor, after which the gateway reports the sandbox as
  Error and refuses `start`. The mapping of OpenShell's Stop/Start onto
  suspend/resume needs the gateway's expectations spelled out (M2).
- The supervisor is not watched: if it exits, the driver still reports Ready.
- Workload DNS: the actor's `/etc/resolv.conf` names kube-dns; OpenShell's
  Docker driver points the workload at the sandbox's relay (127.0.0.53).
  Whether the broker captures DNS to any resolver is not verified yet.
- One actor hosts the guest, the supervisor and the workload; the outer fence
  is the actor's EgressPolicy rather than `network=none`. See the plan, R3.
- Each gateway rollout resets its SQLite state (emptyDir) and orphans the
  environments it created; delete them with `ate-env <id> delete`.
