# Running NVIDIA OpenShell sandboxes on ate-env

[OpenShell](https://github.com/NVIDIA/OpenShell) is NVIDIA's runtime for
autonomous agents: a gateway provisions sandboxes through a compute driver and
enforces a policy on every outbound connection the agent makes. This repo
ships an OpenShell *extension compute driver* that backs each sandbox with an
ate-env environment, that is, a Substrate actor in a microVM. The gateway, the
`openshell` CLI, the SDKs and the policy model stay as they are; only the
compute changes. The code and the install steps live in
[`integrations/openshell`](../integrations/openshell/README.md); this page is
the design: which object becomes which, what runs where, and what differs from
OpenShell's own drivers.

## Object model

| OpenShell | ate-env | Substrate | note |
|---|---|---|---|
| Gateway | none | none | a Deployment of its own; keeps Workspace, Sandbox, Policy, Template and Provider records and holds every supervisor session |
| Compute driver (`openshell-driver-substrate`) | client of ate-env-api through `clients/go` | none today | sidecar in the gateway pod; the only writer of the mapping sandbox id to environment id |
| Workspace | atespace (`ate-env`, shared) | atespace | `EnsureWorkspace` records the name; one atespace serves all workspaces |
| `SandboxTemplate.image` | `rootfs_image` on create | ActorTemplate derived from the base template for that image, with its golden snapshot | digest-pinned images only, see [rootfs-images](rootfs-images/README.md) |
| Sandbox | Environment `os-<sha256(sandbox id)[:20]>` | Actor of the same name, one microVM | the hash keeps the name stable across renames and inside Substrate's name rules |
| Sandbox phase | environment status | actor state | the driver translates into the gateway's condition vocabulary, see Lifecycle |
| `launch_authentication` | files written through the guest | files in the actor rootfs under `/.openshell` | survive suspend and resume, gone with the actor |
| `openshell-sandbox`, `openshell-supervisor`, main process | guest processes | processes in the VM under the ate-env guest (PID 1) | started by the driver, ended by it on stop |
| trusted runtime binaries | runtime layer | ActorTemplate image volume at `/opt/openshell` | one layer image per OpenShell release, see [RUNTIMES.md](rootfs-images/RUNTIMES.md) |
| `SandboxPolicy` network rules | none | EgressPolicy per actor | enforced inside by the supervisor's policy engine and proxy, outside by atenet-egress |
| Stop and Start | `SuspendEnvironment`, resume on the first guest call | memory snapshot to the snapshots bucket, then restore | processes end on stop and relaunch on start |
| Delete | `DeleteEnvironment` | actor deleted; derived template and golden snapshot stay | |
| exec, SSH, port forward, upload, download, logs | not involved | supervisor session out through atenet-egress | ate-env's own exec, file and port-proxy paths stay available to operators |

## Components

```
 openshell CLI / Python SDK / NeMo Gym OpenShell provider
   |  gRPC openshell.v1.OpenShell
   v
+- namespace openshell: Pod openshell-gateway ---------------------------------------------+
|  openshell-gateway                 unix socket           openshell-driver-substrate      |
|  SQLite: Workspace, Sandbox,  <----------------------->  sidecar, Go, clients/go         |
|  Policy, Template, Provider;       openshell.compute.v1  Sandbox id -> environment id    |
|  supervisor sessions;              ComputeDriver         os-<sha256(id)[:20]>            |
|  exec / ssh / forward relay        Create, Stop, Start,                                  |
|                                    Delete, Watch, ...                                    |
+-----^---------------------------------------------------------------+--------------------+
      |  supervisor session: outbound gRPC from the actor            |  gRPC ateenv.v1alpha:
      |  policy, exec, ssh, forwards, logs                           |  Environment, Process,
      |                                                              |  FileSystem services
+-----+-- namespace ate-env -----------------------------------------+---------------------+
|      |                                                            v                      |
|  atenet-egress (Envoy + ext-proc)                        ate-env-api                     |
|  enforces the actor's EgressPolicy                       derives an ActorTemplate        |
|      ^                                                   per image; guest calls go       |
|      |                                                   through the router, which       |
|      |                                                   resumes a suspended actor       |
|      |                                                            |  ateapi              |
|  Substrate objects in atespace ate-env                            v                      |
|   WorkerPool <template>-workerpool  (ateom-microvm workers, sandbox class microvm)       |
|   ActorTemplate <base> --derive per image--> ActorTemplate <image>                       |
|     guest image, volume openshell-layer -> /opt/openshell, capabilities, limits,         |
|     snapshotConfig (golden snapshot per derived template)                                |
|   EgressPolicy os-...  http rule: openshell-gateway.openshell.svc.cluster.local:8080     |
|      |                                                                                   |
|   Actor os-... = one microVM (cloud-hypervisor, kata guest kernel)                       |
|   +----------------------------------------------------------------------------------+   |
|   | PID 1 ate-env-guest (root): StartProcess, WriteFile, captures process output     |   |
|   |   /opt/openshell (layer, ro)   /.openshell/{channel,supervisor}   /sandbox       |   |
|   |   /etc/resolv.conf: actor resolver first, relay 127.0.0.53 second                |   |
|   | openshell-sandbox (workload uid): Landlock + seccomp broker, DNS relay           |   |
|   |   +-- workload: main process, exec and ssh sessions                              |   |
|   | openshell-supervisor (workload uid): policy engine, L7 proxy; dials the gateway -+---+
|   +----------------------------------------------------------------------------------+   |
+------------------------------------------------------------------------------------------+
```

Two arrows cross the namespace boundary and they are the whole integration
surface: driver to ate-env-api for control, and supervisor to gateway through
atenet-egress for data and policy. Nothing OpenShell-specific runs per node;
the node-level pieces are Substrate's own (atelet and the worker pods, which
host many actors each). Upstream's Kubernetes driver spends two pods per
sandbox (workload and supervisor); here both are processes inside the actor.

## How a sandbox comes up

1. `CreateSandbox` arrives with the image, the command, the policy and a
   signed launch bundle. The driver validates (digest-pinned image, known
   workspace) and records the sandbox.
2. The driver calls `CreateEnvironment` with the base template and
   `rootfs_image` set to the sandbox image. ate-env-api derives or reuses the
   per-image template; the actor boots; the driver polls `busybox true`
   through the guest until it answers.
3. A staging script run through the guest sets
   `net.ipv4.ip_unprivileged_port_start=0` (the relay binds 127.0.0.53:53 with
   no capabilities), creates `/.openshell/channel/sandbox`,
   `/.openshell/supervisor`, `/run/openshell`,
   `/run/openshell-supervisor-ca/material` and `/sandbox` owned by the workload
   uid, and rewrites `/etc/resolv.conf` (see below). The driver writes the
   boundary bootstrap, a per-session TLS pair, the runtime descriptor and the
   supervisor's auth bundle.
4. The driver starts `openshell-sandbox launch-capability-free <uid> <gid>
   bootstrap.json /sandbox` (root clears the bounding set, switches to the
   workload identity, sets no_new_privs, then runs the boundary) and
   `openshell-supervisor` with `OPENSHELL_ENDPOINT` set to the gateway URL. It
   checks after three seconds that both still run.
5. The supervisor dials out through atenet-egress. The actor's EgressPolicy
   must allow cleartext HTTP to the gateway host and port. The gateway marks
   the sandbox Ready when the supervisor session is up.

## Lifecycle

The gateway derives a sandbox's phase from a fixed vocabulary of driver
conditions, so the driver speaks it:

| driver state | conditions reported | gateway phase |
|---|---|---|
| creating, resuming | `Ready=False` reason `Starting` | Provisioning |
| both processes running | `Ready=True` reason `BackendReady` | Ready |
| stop done, actor suspended | `Ready=False` reason `ContainerStopped`, `Suspended=True` | Stopped |
| stop requested, actor still suspending | `Suspended=False` reason `PodTerminating` | Stopping |
| launch failed, actor crashed, environment gone | `Ready=False` with a non-transient reason | Error |
| environment deleting | `deleting` flag | Deleting |

`stop` ends the supervisor and the sandbox before it suspends the actor, and
`start` resumes the actor and launches both again from the fresh launch bundle
the gateway minted for that start. Keeping the processes alive across a
suspend is not possible today: the gateway rotates the supervisor's token on
every start and rejects the old one. The filesystem, including `/sandbox`,
survives. A second `stop` is a no-op.

An actor that Substrate suspends for idleness while the sandbox is Ready is
still reported Ready; the router resumes it on the next guest call, but the
supervisor session is gone with the snapshot. Disable idle suspend on the
OpenShell template or expect the gateway to notice a lost supervisor.

## What differs from OpenShell's own drivers

- **One actor hosts guest, supervisor and workload.** Upstream separates the
  supervisor (own pod or container, with network) from the workload (no
  network). Here the outer fence is the actor's EgressPolicy: a workload
  process that escaped seccomp mediation could reach what the policy allows,
  which is the set the supervisor needs. Keep the policy minimal and derived
  from the sandbox policy.
- **One `/etc/resolv.conf`.** Upstream points the workload at the sandbox
  relay (127.0.0.53) only, and the supervisor reads the host resolver. Both
  share the file here (the actor has no CAP_SYS_ADMIN for a private mount
  namespace), so it lists the actor's resolver first (the supervisor's trusted
  upstream) and the relay second: the broker refuses the workload's first
  query at once and glibc falls through. No search list, so cluster-suffixed
  names do not reach the relay.
- **Credentials through the guest.** The auth bundle is written through the
  guest's file API, mode 0600 in a 0700 directory owned by the workload uid.
  Anyone with ate-env-api access can read it, which is the same trust model as
  every other guest call.
- **Capabilities on the actor.** Actors run as root with a minimal capability
  set; OpenShell's launcher needs `SETPCAP, SETUID, SETGID, CHOWN, DAC_OVERRIDE,
  FOWNER` to switch to the workload identity. The workload itself runs with no
  capabilities, as upstream.
- **Kernel.** OpenShell needs Landlock ABI 3 or newer and seccomp
  user-notification with ADDFD. Substrate's microVM class qualifies (kata
  guest kernel 6.18, Landlock ABI 7, `openshell-sandbox capability-probe-launch`
  reports `qualified: true`). The gVisor class does not and is not supported.

## Requirements

- Substrate at or after `0443aec3` (the WorkerPool chooses the sandbox; the
  template names none), and a WorkerPool of the `microvm` class, labeled
  `ate-env/workerpool: <pool>`, whose worker image was built from the same
  Substrate commit as the cluster.
- A base ActorTemplate from `ate-env manifest template --workerpool <pool>
  --layer openshell=<layer>@sha256:…=/opt/openshell --cap-add SETPCAP
  --cap-add SETUID … --cpu 1 --memory 1Gi`. The selector keeps the actors on
  the microvm pool; without it a cold boot can land on a gVisor pool.
- A gateway reachable from the actors' egress path, and an EgressPolicy per
  OpenShell actor that allows `http` to the gateway host and port.
- Images pinned by digest.

## Not done yet

- The driver does not create the EgressPolicy; a helper script does, and it
  can lose the race against the supervisor's first dial (the supervisor
  retries, which costs about ten seconds).
- Egress beyond the gateway is not mapped from the sandbox policy, so a
  policy that allows an external host resolves but cannot connect.
- Driver records live in memory; a driver restart forgets its sandboxes. Each
  gateway rollout with the test manifest also resets the gateway's SQLite
  store and orphans the environments it created.
- The supervisor is not watched; if it exits, the driver still reports Ready.
- Per-create CPU and memory, `workload_identity` from the image user, and
  OpenShell's driver conformance suite in CI.
