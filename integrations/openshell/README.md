# NVIDIA OpenShell on Agent Substrate (ate-env)

Runs [OpenShell](https://github.com/NVIDIA/OpenShell) sandboxes as ate-env
environments. The OpenShell gateway keeps its API, policy model, CLI and SDKs;
the compute driver in this repo backs each sandbox with one Substrate actor in
a microVM, where the two trusted OpenShell binaries (`openshell-sandbox` and
`openshell-supervisor`) run from a runtime layer next to the ate-env guest.
The design, the object model and the component diagram are in
[docs/openshell.md](../../docs/openshell.md); this page is how to install,
use and debug it.

Verified 2026-10-16 on GKE with Substrate `v0.4.0-83-g960997ab`, OpenShell
v0.1.4, kata guest kernel 6.18.35:

| step | result |
|---|---|
| `openshell sandbox create --from debian@sha256:… -- sleep 3600` | Ready 12 to 22 s after the request (12 s when the EgressPolicy is in place before the supervisor dials) |
| `openshell sandbox exec -- id` | `uid=65534(nobody)`, cwd `/sandbox`, unmodified image |
| `openshell sandbox stop` | supervisor and sandbox ended, actor suspended; Stopped after 10 to 13 s; a second `stop` is a no-op |
| `openshell sandbox start` | actor resumed, both binaries relaunched with the fresh credentials; Ready after 12 to 13 s; `/sandbox` kept its files; two cycles in a row |
| `ssh -F <ssh-config> … id`, `ssh -L` to a listener in the sandbox | work through `openshell ssh-proxy` |
| `openshell sandbox upload` / `download` | work |
| `getent ahosts example.com` inside | answered by the sandbox relay with a policy-DNS address (198.18.0.2); names outside the policy are staged for review |
| `openshell sandbox delete` | environment deleted, no `os-*` actor left |
| the same on a gVisor actor | OpenShell's probe fails after the privilege drop (no Landlock); microVM only |

## Layout

| path | what |
|---|---|
| `layer/` | `Dockerfile` for the runtime layer image: `openshell-sandbox` (static musl), `openshell-supervisor` with a bundled glibc and loader behind the `bin/supervisor` wrapper, a static busybox for the staging script. linux/amd64. |
| `../../cmd/openshell-driver-substrate`, `../../internal/openshelldriver` | the compute driver: `openshell.compute.v1.ComputeDriver` over a Unix socket, built on `clients/go`. `bootstrap.go` is the OpenShell v0.1.4 launch contract, `launch.go` stages it in the actor and starts the binaries, `driver.go` the RPCs and the condition mapping (`driver_test.go` checks it against the gateway's phase rules). `third_party/openshell/` holds NVIDIA's protos (Apache-2.0, copied from v0.1.4) and the Go stubs generated from them. |
| `deploy/workerpool.yaml` | a microVM WorkerPool for OpenShell actors. |
| `deploy/gateway.yaml`, `deploy/gateway.toml` | a plaintext, unauthenticated gateway for a test cluster, with the driver as a sidecar sharing the socket volume. |
| `hack/install.sh` | pool, template and gateway in one go; re-runnable. |
| `hack/smoke.sh` | the table above as a script: create, exec, stop, start, ssh, forward, upload, download, delete, leak check. |
| `hack/egress-policies.sh` | gives every `os-*` actor an EgressPolicy to the gateway. Stand-in until the driver creates the policy itself. |
| `examples/agent_loop.py` | the OpenShell Python SDK driving a sandbox as a tool: create with a policy, exec, stop, start, delete. `examples/policy-example.yaml` is the same policy for the CLI. |

## Install

Prerequisites: a cluster with ate-env deployed (`ate-env manifest … | kubectl apply`)
on Substrate at or after `63a17a92` (the generated template names its sandbox
class as the string `microvm` and declares limits, as the rest of ate-env does
since #91; on an older cluster write the enum form `SANDBOX_CLASS_MICROVM` by
hand), a `microvm` SandboxConfig (Substrate's `ate-setup` installs one),
`kubectl`, `kubectl-ate`, `ate-env`, and the `openshell` CLI on your machine.

1. **Images.** `make openshell-images` builds and pushes the layer (docker
   buildx, linux/amd64) and the driver (ko) into `$ATE_ENV_IMAGE_REPO` and
   prints both digests. The worker image (`ateworker-microvm`; `ateom-microvm`
   before Substrate `63a17a92`) comes from the Substrate repo at the commit
   the cluster runs: `KO_DOCKER_REPO=<repo> go run ./cmd/ate-setup publish
   worker-images` there. A worker image older than the cluster fails its
   capacity report with `default_runtime.version: Required value`.
2. **Pool, template, gateway.**

   ```bash
   LAYER_IMAGE=<repo>/openshell-layer@sha256:… \
   GUEST_IMAGE=<repo>/ate-env-guest@sha256:… \
   WORKER_IMAGE=<repo>/ateom-microvm@sha256:… \
   SNAPSHOTS_BUCKET=gs://<bucket>/ate-env/ \
   DRIVER_IMAGE=<repo>/openshell-driver-substrate@sha256:… \
   CONTEXT=<kube context> integrations/openshell/hack/install.sh
   ```

   The script applies `deploy/workerpool.yaml` with your image and replica
   count, registers the ActorTemplate `openshell-microvm` with

   ```bash
   ate-env manifest template --template openshell-microvm \
     --guest-image "$GUEST_IMAGE" --snapshots-bucket "$SNAPSHOTS_BUCKET" \
     --layer openshell="$LAYER_IMAGE"=/opt/openshell \
     --sandbox-class microvm --sandbox-config microvm \
     --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
     --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
     --cpu 1 --memory 1Gi | kubectl-ate create actor-template -f -
   ```

   and applies `deploy/gateway.yaml` (namespace `openshell`, one replica,
   SQLite on an emptyDir, plaintext API) with the driver image you pass. The
   capabilities are what OpenShell's capability-free launcher needs to switch
   from root to the workload identity; actors otherwise run with only KILL,
   NET_BIND_SERVICE and AUDIT_WRITE. The template, pool and worker class must
   agree (`microvm`), or actors never schedule.
3. **Reach the gateway and let actors reach it.**

   ```bash
   kubectl -n openshell port-forward svc/openshell-gateway 18080:8080 &
   openshell gateway add --name substrate http://127.0.0.1:18080 && openshell gateway select substrate
   CONTEXT=<kube context> integrations/openshell/hack/egress-policies.sh &
   ```

   The helper creates, for every new `os-*` actor, an EgressPolicy with one
   `http` rule for `openshell-gateway.openshell.svc.cluster.local:8080`. The
   supervisor's gRPC (h2c) passes that rule; by IP it is refused.

## Use

```bash
openshell sandbox create --name t1 --from docker.io/library/debian@sha256:<digest> \
  --detach --no-tty --no-auto-providers -- sleep 3600
openshell sandbox exec t1 -- id
openshell sandbox stop t1 && openshell sandbox start t1
openshell sandbox exec t1 -- ls /sandbox
openshell sandbox delete t1
```

Images must be pinned by digest: ate-env derives one template per image and
does not resolve tags. `--policy examples/policy-example.yaml` attaches a
network policy; inside the sandbox, `getent ahosts example.com` then answers
with a policy-DNS address and other names are staged for review.

The whole table above as one command, with timings:

```bash
IMAGE=docker.io/library/debian@sha256:<digest> CONTEXT=<kube context> integrations/openshell/hack/smoke.sh
```

From Python, with the OpenShell SDK (uv installs it from the script's inline
metadata):

```bash
uv run integrations/openshell/examples/agent_loop.py --image docker.io/library/debian@sha256:<digest>
```

## Lifecycle

| you run | the driver does | Substrate side |
|---|---|---|
| `create` | `CreateEnvironment` from `openshell-microvm` with `rootfs_image`; stages files; starts `openshell-sandbox` and `openshell-supervisor` | derived template per image, golden snapshot, one actor |
| `stop` | kills both processes (their process groups), `SuspendEnvironment`; reports `Suspended=True` | memory snapshot to the bucket; the actor's slot is free |
| `start` | resumes the actor with a guest call, stages the fresh launch bundle, starts both again | restore from the snapshot |
| `delete` | `DeleteEnvironment` | actor gone; derived template and golden snapshot stay |

The processes cannot stay alive across a stop: the gateway rotates the
supervisor's token on every start and rejects the old one. Files survive.
A sandbox that Substrate suspends for idleness while Ready stays Ready in the
gateway until the gateway notices the lost supervisor; disable idle suspend on
the template if that matters. The driver's condition vocabulary and the
gateway phases are listed in [docs/openshell.md](../../docs/openshell.md#lifecycle).

## Troubleshooting

| symptom | cause | fix |
|---|---|---|
| `create` fails with `no worker has room` | no ready microVM worker: wrong class, missing `resources.limits`, or the worker image predates the cluster's Substrate | `kubectl -n ate-env get workerpool`, check the capacity report in the worker logs; rebuild `ateom-microvm` from the cluster's commit |
| `default_runtime.version: Required value` in worker logs | worker image older than the cluster's API | same |
| `drop capability 0: EPERM` in the driver's `LaunchFailed` message | template without the `--cap-add` set | recreate the template with `FORCE=1 hack/install.sh` |
| `bind socket probe DNS relay: Permission denied` | `net.ipv4.ip_unprivileged_port_start` not lowered | the staging script sets it; check `/proc/sys` is writable in the actor |
| sandbox stays Provisioning; supervisor log says `Startup configuration fetch failed, retrying` | the actor has no EgressPolicy yet, or the gateway endpoint lacks its `http://` scheme | run `hack/egress-policies.sh`; check `--gateway-endpoint` |
| `h2 protocol error` on `create` | an oversized gRPC status from the driver | fixed; the driver caps error text at 12 lines |
| `relay stream: inbound errored … h2 protocol error` in the gateway log after each exec | the relay stream ends with a reset through atenet-egress | harmless so far; output arrives |
| sandboxes vanish after a gateway rollout | the test manifest keeps SQLite on an emptyDir | delete the orphaned `os-*` environments with `ate-env <id> delete`; give the gateway a volume for anything beyond a test |
| `openshell sandbox list` shows Error right after `stop` | an old driver reported a reason the gateway treats as terminal | update the driver image; the condition mapping is covered by `driver_test.go` |

Supervisor and sandbox output is captured by the guest. The driver sidecar's
log prints the two process ids per sandbox (`"processes":[…]`); stream them
with the Go client's `Process(id).Output` through `ate-env-api`.

## Development

```bash
make build vet test verify-boilerplate verify-gofmt   # Go, incl. the driver and the manifest flags
make openshell-images                                   # layer (buildx) + driver (ko)
(cd internal/openshelldriver/third_party/openshell/proto && buf generate)   # after bumping OpenShell
```

Bumping OpenShell: copy `proto/*.proto` from the new tag into
`third_party/openshell/proto/openshell/`, keep the one local change (the
`MethodOptions` extension `authorization = 50000` stays out of `options.proto`
because `go.ate.dev/substrate` registers `ateapi.method` with the same number
and the Go protobuf registry panics on the pair; see
`third_party/openshell/README.md`), regenerate, update the two tarball
digests in `layer/Dockerfile`, and re-read the launch contract in
`bootstrap.go` against `crates/openshell-sandbox-backend` and the gateway's
`derive_phase`; `driver_test.go` mirrors the latter.

## Known gaps

- The driver does not create the actor's EgressPolicy; the helper script does,
  and it can lose the race against the supervisor's first dial (the
  supervisor retries, which costs about ten seconds). Next step: the driver
  creates the policy through Substrate's ateapi before it starts the
  supervisor, and derives rules for the hosts the sandbox policy allows. Today
  a policy that allows `example.com:443` resolves but cannot connect. Note
  that OpenShell itself refuses a raw TCP connect to a `protocol: rest`
  endpoint (`transparent_tcp_policy_denied`); test the mapping with an HTTP
  client or a `tcp` rule.
- Driver records live in memory; a driver restart forgets its sandboxes.
- The supervisor is not watched: if it exits, the driver still reports Ready.
- Workload DNS rides on one `/etc/resolv.conf` for the whole actor (the
  actor's resolver first for the supervisor, the relay 127.0.0.53 second for
  the workload); OpenShell's drivers give the workload the relay only. A
  private mount namespace would need CAP_SYS_ADMIN on the actor.
- One actor hosts the guest, the supervisor and the workload; the outer fence
  is the actor's EgressPolicy rather than `network=none`.
- Per-create CPU and memory, `workload_identity` from the image user, and the
  OpenShell driver conformance suite in CI.
