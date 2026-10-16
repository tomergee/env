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

# Installs the OpenShell runtime on a cluster that already runs ate-env:
# a microVM worker pool, the base ActorTemplate with the OpenShell layer, and
# the gateway with the Substrate driver as its sidecar. Safe to re-run: an
# existing pool is updated, an existing template is kept unless FORCE=1, and
# the gateway is re-applied.
#
# Required:  LAYER_IMAGE, GUEST_IMAGE, SNAPSHOTS_BUCKET, WORKER_IMAGE
# Optional:  CONTEXT, ATESPACE, NAMESPACE, GATEWAY_NAMESPACE, TEMPLATE,
#            WORKERPOOL, SANDBOX_CONFIG, REPLICAS, CPU, MEMORY, DRIVER_IMAGE,
#            ATE_ENV, KUBECTL, KUBECTL_ATE, FORCE
set -euo pipefail

: "${LAYER_IMAGE:?set LAYER_IMAGE to the digest-pinned openshell-layer image}"
: "${GUEST_IMAGE:?set GUEST_IMAGE to the digest-pinned ate-env-guest image}"
: "${SNAPSHOTS_BUCKET:?set SNAPSHOTS_BUCKET to the snapshot object-storage URL, e.g. gs://bucket/ate-env/}"
: "${WORKER_IMAGE:?set WORKER_IMAGE to the digest-pinned ateom-microvm image built from the Substrate commit the cluster runs}"
ATESPACE=${ATESPACE:-ate-env}
NAMESPACE=${NAMESPACE:-ate-env}
GATEWAY_NAMESPACE=${GATEWAY_NAMESPACE:-openshell}
TEMPLATE=${TEMPLATE:-openshell-microvm}
WORKERPOOL=${WORKERPOOL:-${TEMPLATE}-workerpool}
SANDBOX_CONFIG=${SANDBOX_CONFIG:-microvm}
REPLICAS=${REPLICAS:-2}
CPU=${CPU:-1}
MEMORY=${MEMORY:-1Gi}
ATE_ENV=${ATE_ENV:-ate-env}
KUBECTL=${KUBECTL:-kubectl}
KUBECTL_ATE=${KUBECTL_ATE:-kubectl-ate}
ctx=()
[ -n "${CONTEXT:-}" ] && ctx=(--context "$CONTEXT")
here=$(cd "$(dirname "$0")/.." && pwd)

step() { printf '\n== %s\n' "$*"; }

step "SandboxConfig ${SANDBOX_CONFIG} (the microVM class the pool and the template name)"
if ! "$KUBECTL" "${ctx[@]}" get sandboxconfigs.ate.dev "$SANDBOX_CONFIG" >/dev/null 2>&1; then
  echo "warning: no SandboxConfig named ${SANDBOX_CONFIG} found; the pool will not become ready without one" >&2
fi

step "WorkerPool ${NAMESPACE}/${WORKERPOOL}: ${REPLICAS} x ${WORKER_IMAGE}"
sed -e "s|^  name: .*|  name: ${WORKERPOOL}|" \
    -e "s|^  namespace: .*|  namespace: ${NAMESPACE}|" \
    -e "s|^  replicas: .*|  replicas: ${REPLICAS}|" \
    -e "s|^      name: .*|      name: ${SANDBOX_CONFIG}|" \
    -e "s|^  workerImage: .*|  workerImage: ${WORKER_IMAGE}|" \
    "$here/deploy/workerpool.yaml" | "$KUBECTL" "${ctx[@]}" apply -f -

step "ActorTemplate ${ATESPACE}/${TEMPLATE}: guest + OpenShell layer, class microvm, ${CPU} CPU, ${MEMORY}"
if "$KUBECTL_ATE" "${ctx[@]}" get actor-template "$TEMPLATE" -a "$ATESPACE" >/dev/null 2>&1 && [ "${FORCE:-0}" != 1 ]; then
  echo "template exists; set FORCE=1 to delete and recreate it (derived templates keep the old settings until recreated)"
else
  if [ "${FORCE:-0}" = 1 ]; then
    "$KUBECTL_ATE" "${ctx[@]}" delete actor-template "$TEMPLATE" -a "$ATESPACE" >/dev/null 2>&1 || true
  fi
  "$ATE_ENV" manifest template --template "$TEMPLATE" --atespace "$ATESPACE" \
    --guest-image "$GUEST_IMAGE" --snapshots-bucket "$SNAPSHOTS_BUCKET" \
    --layer "openshell=${LAYER_IMAGE}=/opt/openshell" \
    --sandbox-class microvm --sandbox-config "$SANDBOX_CONFIG" \
    --cap-add SETPCAP --cap-add SETUID --cap-add SETGID --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
    --cpu "$CPU" --memory "$MEMORY" \
    | "$KUBECTL_ATE" "${ctx[@]}" create actor-template -f -
fi

step "Gateway in namespace ${GATEWAY_NAMESPACE} with the driver sidecar"
"$KUBECTL" "${ctx[@]}" apply -f "$here/deploy/gateway.yaml"
"$KUBECTL" "${ctx[@]}" -n "$GATEWAY_NAMESPACE" set env deploy/openshell-gateway -c driver \
  "OPENSHELL_BASE_TEMPLATE=${TEMPLATE}" >/dev/null
if [ -n "${DRIVER_IMAGE:-}" ]; then
  "$KUBECTL" "${ctx[@]}" -n "$GATEWAY_NAMESPACE" set image deploy/openshell-gateway "driver=${DRIVER_IMAGE}" >/dev/null
fi
"$KUBECTL" "${ctx[@]}" -n "$GATEWAY_NAMESPACE" rollout status deploy/openshell-gateway --timeout=180s

cat <<MSG

Done. Next:
  ${KUBECTL} ${ctx[*]:-} -n ${GATEWAY_NAMESPACE} port-forward svc/openshell-gateway 18080:8080 &
  openshell gateway add --name substrate http://127.0.0.1:18080 && openshell gateway select substrate
  CONTEXT=${CONTEXT:-} ATESPACE=${ATESPACE} ${here}/hack/egress-policies.sh &   # until the driver creates the policy itself
  IMAGE=docker.io/library/debian@sha256:<digest> ${here}/hack/smoke.sh
MSG
