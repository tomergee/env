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

# A runtime layer that carries NVIDIA OpenShell's two trusted sandbox
# binaries, for mounting read-only into unmodified images at /opt/openshell
# (docs/rootfs-images/RUNTIMES.md). It brings everything it runs on:
#   - openshell-sandbox:    static musl, from the release tarball;
#   - openshell-supervisor: glibc-linked (needs 2.28+), run through the loader
#                           and libraries copied here from Debian bookworm, so
#                           it does not depend on the task image's libc;
#   - busybox (static):     setuidgid, mkdir, chown and friends for the launch
#                           wrapper, independent of what the image ships.
# linux/amd64 only; both OpenShell artefacts are verified against the sums
# published with the release.
#   docker buildx build --platform linux/amd64 \
#     -t <registry>/openshell-layer:0.1.4 --push integrations/openshell/layer

ARG OPENSHELL_VERSION=0.1.4
ARG OPENSHELL_SANDBOX_SHA256=832b65c7af487ddbf57cf84cc0edd55de8b7bdef021c6e9e93b0ee8c5acd1571
ARG OPENSHELL_SUPERVISOR_SHA256=f622fff9cb645827875d9b795cf24b5b65cb04e705c2593125250c0db221cea4

FROM --platform=$BUILDPLATFORM curlimages/curl:8.16.0 AS fetch
ARG OPENSHELL_VERSION
ARG OPENSHELL_SANDBOX_SHA256
ARG OPENSHELL_SUPERVISOR_SHA256
WORKDIR /tmp/fetch
RUN set -eu; base="https://github.com/NVIDIA/OpenShell/releases/download/v${OPENSHELL_VERSION}"; \
    curl -fsSLo sandbox.tgz    "$base/openshell-sandbox-x86_64-unknown-linux-musl.tar.gz"; \
    curl -fsSLo supervisor.tgz "$base/openshell-supervisor-x86_64-unknown-linux-gnu.tar.gz"; \
    echo "${OPENSHELL_SANDBOX_SHA256}  sandbox.tgz"       | sha256sum -c -; \
    echo "${OPENSHELL_SUPERVISOR_SHA256}  supervisor.tgz" | sha256sum -c -; \
    mkdir -p out/bin && tar -xzf sandbox.tgz -C out/bin && tar -xzf supervisor.tgz -C out/bin && \
    chmod 0555 out/bin/openshell-sandbox out/bin/openshell-supervisor

# glibc 2.36 and the loader, for the supervisor. Digest-pinned amd64 image.
FROM --platform=linux/amd64 debian:bookworm-slim@sha256:a4672c0cb26fbdde88e38fa2dfb6c681942306680e41e4378b28770b6e79ee91 AS libs
# static busybox (musl), digest-pinned amd64 image
FROM --platform=linux/amd64 busybox:1.37-musl@sha256:dc88b80842580654294472735332ec7c7789a4010ec9a68b69f9b88c19abede6 AS busybox

FROM scratch
COPY --from=fetch /tmp/fetch/out/bin/ /bin/
COPY --from=libs /usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2 \
                 /usr/lib/x86_64-linux-gnu/libc.so.6 \
                 /usr/lib/x86_64-linux-gnu/libm.so.6 \
                 /usr/lib/x86_64-linux-gnu/libdl.so.2 \
                 /usr/lib/x86_64-linux-gnu/libpthread.so.0 \
                 /usr/lib/x86_64-linux-gnu/librt.so.1 \
                 /usr/lib/x86_64-linux-gnu/libgcc_s.so.1 \
                 /usr/lib/x86_64-linux-gnu/libresolv.so.2 \
                 /usr/lib/x86_64-linux-gnu/libnss_dns.so.2 \
                 /usr/lib/x86_64-linux-gnu/libnss_files.so.2 /lib/
COPY --from=busybox /bin/busybox /bin/busybox
COPY --chmod=0555 supervisor /bin/supervisor
