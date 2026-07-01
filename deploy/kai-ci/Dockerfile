# syntax=docker/dockerfile:1
#
# kai-ci — toolbox image for running the kai CLI inside CI workflow pods
# (kai capture / kai review analyze / curl ingest). The kai-server review
# workflow (.kailab/workflows/kai-review.yml) points its job container.image
# at registry.kaicontext.com/kai-ci:latest.
#
# BUILD CONTEXT must be the workspace root that holds the three module dirs
# side by side (kai-cli/, kai-core/, kai-engine/), because `kai review analyze`
# depends on kai-engine packages (finding, shouldtouch, safetygate/reachback)
# that are not in the published kai-engine tag yet. Build with build.sh, or:
#
#   cd <workspace-root>            # dir containing kai-cli/ kai-core/ kai-engine/
#   docker buildx build --platform linux/amd64 \
#     -f kai-cli/deploy/kai-ci/Dockerfile \
#     -t registry.kaicontext.com/kai-ci:latest .
#
# The builder runs NATIVELY (BUILDPLATFORM) and CROSS-compiles the CGO binary
# to the target arch — compiling under qemu emulation crashes the Go toolchain
# ("signal: illegal instruction"), so we never emulate the compile.

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS builder
ARG TARGETARCH
# tree-sitter is a CGO binding, so we need a C compiler for the TARGET arch.
# (sqlite is modernc/pure-Go and needs none.) Install the cross-gcc when the
# target differs from the native build arch.
# NB: the cross gcc alone is not enough for CGO — the TARGET libc dev headers
# (libc6-dev-<arch>-cross) are required, and --no-install-recommends omits them.
RUN set -eux; \
    apt-get update; \
    case "$TARGETARCH" in \
      amd64) apt-get install -y --no-install-recommends gcc-x86-64-linux-gnu libc6-dev-amd64-cross ;; \
      arm64) apt-get install -y --no-install-recommends gcc-aarch64-linux-gnu libc6-dev-arm64-cross ;; \
    esac; \
    rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY kai-cli/    kai-cli/
COPY kai-core/   kai-core/
COPY kai-engine/ kai-engine/
# Build against local sources via replace directives (deterministic — overrides
# the pinned kai-engine v0.1.3 / kai-core v0.1.0 with the checked-out code, at
# any version). CC points at the target-arch cross compiler for CGO.
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) CC=x86_64-linux-gnu-gcc ;; \
      arm64) CC=aarch64-linux-gnu-gcc ;; \
      *) CC=gcc ;; \
    esac; \
    cd kai-cli; \
    go mod edit -replace github.com/kaicontext/kai-engine=../kai-engine; \
    go mod edit -replace github.com/kaicontext/kai-core=../kai-core; \
    CC="$CC" CGO_ENABLED=1 GOOS=linux GOARCH="$TARGETARCH" \
      go build -trimpath -ldflags="-w -s" -o /out/kai ./cmd/kai

FROM debian:bookworm-slim
# Tools the review workflow invokes: git (checkout), jq (JSON assembly),
# curl (ingest POST), ca-certificates (HTTPS to kailab). bash ships in the base.
# Retry the install: this layer runs under emulation on an arm64 host, where
# dpkg/perl postinst can transiently fail; a native amd64 build never sees this.
RUN set -eux; export DEBIAN_FRONTEND=noninteractive; \
    for i in 1 2 3; do \
      apt-get update && apt-get install -y --no-install-recommends \
        git jq curl ca-certificates && break; \
      echo "apt attempt $i failed; retrying"; sleep 3; \
    done; \
    command -v git && command -v jq && command -v curl; \
    rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/kai /usr/local/bin/kai
WORKDIR /workspace
CMD ["/bin/bash"]
