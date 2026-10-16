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

# End-to-end smoke test of OpenShell on ate-env through the openshell CLI:
# create, exec, stop, start, exec again (the workspace survived), SSH, an SSH
# port forward, upload and download, delete, and a check that no actor was
# left behind. Prints one timing line per step and exits non-zero on the
# first failure. The sandbox is deleted on exit either way.
#
# Required:  IMAGE  (digest-pinned, e.g. docker.io/library/debian@sha256:...)
# Optional:  GATEWAY (openshell gateway name, default substrate), WORKSPACE,
#            NAME, OPENSHELL, SKIP_SSH=1, CONTEXT + ATESPACE + KUBECTL_ATE for
#            the leak check (skipped when kubectl-ate is absent)
set -euo pipefail

: "${IMAGE:?set IMAGE to a digest-pinned rootfs image}"
GATEWAY=${GATEWAY:-substrate}
WORKSPACE=${WORKSPACE:-default}
NAME=${NAME:-smoke-$(date +%H%M%S)}
OPENSHELL=${OPENSHELL:-openshell}
ATESPACE=${ATESPACE:-ate-env}
KUBECTL_ATE=${KUBECTL_ATE:-kubectl-ate}
ctx=()
[ -n "${CONTEXT:-}" ] && ctx=(--context "$CONTEXT")
os=("$OPENSHELL" --gateway "$GATEWAY" --workspace "$WORKSPACE")
tmp=$(mktemp -d)
failed=0

cleanup() {
  "${os[@]}" sandbox delete "$NAME" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

now() { date +%s; }
step() { printf '%-44s' "$*"; }
ok() { printf 'ok   %3ss\n' "$1"; }
fail() { printf 'FAIL %s\n' "$*"; failed=1; exit 1; }
phase() { "${os[@]}" sandbox list 2>/dev/null | awk -v n="$NAME" '$1==n {print $NF}'; }
wait_phase() { # wait_phase <phase> <seconds>
  local deadline=$(( $(now) + $2 ))
  while [ "$(now)" -lt "$deadline" ]; do
    [ "$(phase)" = "$1" ] && return 0
    sleep 2
  done
  fail "sandbox is $(phase), wanted $1 within $2s"
}

echo "sandbox ${NAME} from ${IMAGE} via gateway ${GATEWAY}"

step "create, wait Ready"; t=$(now)
"${os[@]}" sandbox create --name "$NAME" --from "$IMAGE" --detach --no-tty --no-auto-providers -- sleep 3600 >/dev/null
wait_phase Ready 120; ok $(( $(now) - t ))

step "exec id, write marker"; t=$(now)
id=$("${os[@]}" sandbox exec "$NAME" -- id)
case "$id" in uid=*) ;; *) fail "exec id returned: $id";; esac
"${os[@]}" sandbox exec "$NAME" -- touch /sandbox/marker >/dev/null; ok $(( $(now) - t ))

step "stop, wait Stopped"; t=$(now)
"${os[@]}" sandbox stop "$NAME" >/dev/null
wait_phase Stopped 60; ok $(( $(now) - t ))

step "stop again (no-op)"; t=$(now)
"${os[@]}" sandbox stop "$NAME" >/dev/null; [ "$(phase)" = Stopped ] || fail "phase $(phase)"; ok $(( $(now) - t ))

step "start, wait Ready"; t=$(now)
"${os[@]}" sandbox start "$NAME" >/dev/null
wait_phase Ready 120; ok $(( $(now) - t ))

step "marker survived the stop"; t=$(now)
"${os[@]}" sandbox exec "$NAME" -- test -f /sandbox/marker || fail "/sandbox/marker is gone"; ok $(( $(now) - t ))

step "upload, download round trip"; t=$(now)
echo "round trip $NAME" > "$tmp/up.txt"
"${os[@]}" sandbox upload --no-git-ignore "$NAME" "$tmp/up.txt" /sandbox/ >/dev/null 2>&1
mkdir -p "$tmp/dl"; "${os[@]}" sandbox download "$NAME" /sandbox/up.txt "$tmp/dl/" >/dev/null 2>&1
cmp -s "$tmp/up.txt" "$tmp/dl/up.txt" || fail "downloaded file differs"; ok $(( $(now) - t ))

if [ "${SKIP_SSH:-0}" != 1 ] && command -v ssh >/dev/null; then
  step "ssh command through ssh-proxy"; t=$(now)
  "${os[@]}" sandbox ssh-config "$NAME" > "$tmp/ssh.cfg"
  host=$(awk '/^Host /{print $2; exit}' "$tmp/ssh.cfg")
  out=$(ssh -F "$tmp/ssh.cfg" -o BatchMode=yes -o ConnectTimeout=30 "$host" id 2>&1) || fail "ssh: $out"
  case "$out" in uid=*) ;; *) fail "ssh id returned: $out";; esac; ok $(( $(now) - t ))

  step "ssh -L to a listener in the sandbox"; t=$(now)
  if ! "${os[@]}" sandbox exec "$NAME" -- perl -v >/dev/null 2>&1; then
    echo "skip (no perl in the image)"
  else
    "${os[@]}" sandbox exec "$NAME" -- perl -MIO::Socket::INET -e \
      'my $l=IO::Socket::INET->new(LocalAddr=>"127.0.0.1",LocalPort=>8099,Listen=>5,Reuse=>1) or die; while(my $c=$l->accept){print $c "hello from sandbox\n"; close $c}' \
      >/dev/null 2>&1 &
    listener=$!
    ssh -F "$tmp/ssh.cfg" -o BatchMode=yes -N -L 18099:127.0.0.1:8099 "$host" 2>/dev/null &
    fwd=$!
    got=""
    for _ in $(seq 1 15); do got=$( (echo | nc -w 3 127.0.0.1 18099) 2>/dev/null || true); [ -n "$got" ] && break; sleep 1; done
    kill "$fwd" "$listener" 2>/dev/null || true; wait "$fwd" "$listener" 2>/dev/null || true
    [ "$got" = "hello from sandbox" ] || fail "forward returned: $got"; ok $(( $(now) - t ))
  fi
fi

step "delete, wait gone"; t=$(now)
"${os[@]}" sandbox delete "$NAME" >/dev/null
deadline=$(( $(now) + 60 ))
while [ -n "$(phase)" ] && [ "$(now)" -lt "$deadline" ]; do sleep 2; done
[ -z "$(phase)" ] || fail "sandbox still listed as $(phase)"; ok $(( $(now) - t ))

if command -v "$KUBECTL_ATE" >/dev/null 2>&1; then
  step "no os-* actor left in ${ATESPACE}"; t=$(now)
  sleep 3
  left=$("$KUBECTL_ATE" "${ctx[@]}" get actors -a "$ATESPACE" 2>/dev/null | grep -c "os-" || true)
  [ "$left" = 0 ] || fail "$left os-* actor(s) remain"; ok $(( $(now) - t ))
fi
trap - EXIT; rm -rf "$tmp"
echo "all steps passed"
