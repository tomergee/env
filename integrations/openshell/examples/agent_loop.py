# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# /// script
# requires-python = ">=3.11"
# dependencies = ["openshell==0.1.2"]
# ///
"""An agent loop against an OpenShell gateway backed by ate-env.

The shape RL rollouts and tool-using agents use: the model runs outside, the
sandbox is a tool. Nothing here is specific to Substrate; the same script
works against any OpenShell gateway. Run it with uv, which installs the
OpenShell SDK from the inline metadata:

    kubectl -n openshell port-forward svc/openshell-gateway 18080:8080 &
    uv run integrations/openshell/examples/agent_loop.py \
        --image docker.io/library/debian@sha256:<digest>

Steps: create a sandbox from a digest-pinned image with a policy that allows
example.com, run a few "tool calls" (shell commands) and read their output,
stop the sandbox, start it again, show that the workspace survived, delete.
"""

from __future__ import annotations

import argparse
import sys
import time

from openshell import SandboxClient
from openshell._proto import openshell_pb2, sandbox_pb2


def example_policy() -> sandbox_pb2.SandboxPolicy:
    """The same policy as policy-example.yaml, built from the protobuf types.

    The CLI's YAML loader maps `enforcement: enforce` and `access: read-only`
    onto these enums; the SDK takes the message directly.
    """
    policy = sandbox_pb2.SandboxPolicy(version=1)
    policy.filesystem.include_workdir = True
    rule = policy.network_policies["example"]
    rule.name = "example-com"
    endpoint = rule.endpoints.add()
    endpoint.host = "example.com"
    endpoint.port = 443
    endpoint.protocol = "rest"
    endpoint.enforcement = sandbox_pb2.NETWORK_ENFORCEMENT_MODE_ENFORCE
    endpoint.access = sandbox_pb2.NETWORK_ACCESS_PRESET_READ_ONLY
    return policy


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--gateway", default="127.0.0.1:18080", help="gateway host:port (plaintext)")
    ap.add_argument("--workspace", default="default")
    ap.add_argument("--image", required=True, help="digest-pinned rootfs image")
    ap.add_argument("--name", default=f"loop-{int(time.time()) % 100000}")
    ap.add_argument("--keep", action="store_true", help="do not delete the sandbox at the end")
    args = ap.parse_args()

    spec = openshell_pb2.SandboxSpec(
        template=openshell_pb2.SandboxTemplate(image=args.image),
        command=["sleep", "3600"],  # the main process; tool calls run next to it
        tty=False,
        policy=example_policy(),
    )

    def timed(label: str):
        start = time.monotonic()

        def done(extra: str = "") -> None:
            print(f"{label:<36} {time.monotonic() - start:5.1f}s  {extra}")

        return done

    with SandboxClient(args.gateway, timeout=120.0) as client:
        done = timed("create + wait ready")
        ref = client.create(workspace=args.workspace, spec=spec, name=args.name)
        client.wait_ready(ref.name, workspace=args.workspace, timeout_seconds=180)
        done(f"sandbox {ref.name} id {ref.id}")

        def tool(command: list[str]) -> str:
            done = timed("  tool: " + " ".join(command)[:28])
            result = client.exec(ref.name, command, workspace=args.workspace, timeout_seconds=60)
            done(f"exit {result.exit_code}")
            if result.exit_code != 0:
                print("    stderr:", result.stderr.strip()[:300])
            return result.stdout

        # A few turns of an agent: look around, write a result file, check DNS
        # goes through the policy relay (synthetic 198.18.x.x address).
        print("  ", tool(["id"]).strip())
        tool(["sh", "-c", "echo 'answer: 42' > /sandbox/result.txt"])
        print("  ", tool(["cat", "/sandbox/result.txt"]).strip())
        print("  ", (tool(["getent", "ahosts", "example.com"]).strip().splitlines() or ["(no answer)"])[0])

        done = timed("stop + wait stopped")
        client.stop(ref.name, workspace=args.workspace)
        client.wait_stopped(ref.name, workspace=args.workspace, timeout_seconds=120)
        done()

        done = timed("start + wait ready")
        client.start(ref.name, workspace=args.workspace)
        client.wait_ready(ref.name, workspace=args.workspace, timeout_seconds=180)
        done()

        print("  ", "after restart:", tool(["cat", "/sandbox/result.txt"]).strip())

        if args.keep:
            print(f"kept sandbox {ref.name}; delete it with: openshell sandbox delete {ref.name}")
            return 0
        done = timed("delete")
        client.delete(ref.name, workspace=args.workspace)
        done()
    return 0


if __name__ == "__main__":
    sys.exit(main())
