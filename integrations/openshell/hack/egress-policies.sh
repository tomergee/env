#!/usr/bin/env bash
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

# Stand-in for the driver's egress step (M1): give every OpenShell actor
# ("os-*" in the atespace) an EgressPolicy that allows cleartext HTTP, and so
# gRPC over h2c, to the gateway. Polls until interrupted.
#   ATESPACE (ate-env)  GATEWAY_HOST (openshell-gateway.openshell.svc.cluster.local)
#   GATEWAY_PORT (8080) KUBECTL_ATE (kubectl-ate)  CONTEXT (optional kubeconfig context)
set -euo pipefail
ATESPACE=${ATESPACE:-ate-env}
GATEWAY_HOST=${GATEWAY_HOST:-openshell-gateway.openshell.svc.cluster.local}
GATEWAY_PORT=${GATEWAY_PORT:-8080}
KUBECTL_ATE=${KUBECTL_ATE:-kubectl-ate}
ctx=()
[ -n "${CONTEXT:-}" ] && ctx=(--context "$CONTEXT")
policy=$(mktemp)
cat > "$policy" <<YAML
rules:
- http:
    hostnames:
    - ${GATEWAY_HOST}
    ports:
      numbers:
      - ${GATEWAY_PORT}
YAML
while true; do
  for actor in $("$KUBECTL_ATE" "${ctx[@]}" get actors -a "$ATESPACE" -o json 2>/dev/null \
      | python3 -c 'import json,sys
d=json.load(sys.stdin)
items=d if isinstance(d,list) else (d.get("items") or d.get("actors") or [])
[print(a["metadata"]["name"]) for a in items if a.get("metadata",{}).get("name","").startswith("os-")]' 2>/dev/null); do
    # "get" exits 0 and prints a sentence when the actor has no policy yet.
    if "$KUBECTL_ATE" "${ctx[@]}" get egress-policy "$actor" -a "$ATESPACE" 2>&1 | grep -q "has no egress policy"; then
      if out=$("$KUBECTL_ATE" "${ctx[@]}" create egress-policy "$actor" -a "$ATESPACE" -f "$policy" 2>&1); then
        echo "$(date +%T) egress policy created for $actor"
      else
        echo "$(date +%T) egress policy for $actor failed: $out"
      fi
    fi
  done
  sleep 3
done
