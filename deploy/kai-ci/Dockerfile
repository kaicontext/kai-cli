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
#   docker build -f kai-cli/deploy/kai-ci/Dockerfile \
#     -t registry.kaicontext.com/kai-ci:latest .
#
# Once kai-engine publishes those packages and kai-cli's go.mod is bumped, this
# can collapse to a single-module GOWORK=off build inside the kai-cli repo.

# Pin the builder to amd64 so it native-compiles the target arch (on an arm64
# host, Docker emulates amd64 — needed because CGO can't easily cross-compile).
FROM --platform=linux/amd64 golang:1.25-alpine AS builder
# build-base (gcc/musl-dev) is required: the tree-sitter parser is a CGO binding.
RUN apk add --no-cache git ca-certificates build-base
WORKDIR /src
COPY kai-cli/    kai-cli/
COPY kai-core/   kai-core/
COPY kai-engine/ kai-engine/
# Build against local sources via a fresh workspace (overrides the pinned
# kai-engine v0.1.3 / kai-core v0.1.0 with the checked-out code). CGO is on for
# tree-sitter; sqlite (modernc) is pure Go. Native amd64 build, no cross-compile.
RUN go work init ./kai-cli ./kai-core ./kai-engine \
 && CGO_ENABLED=1 go build -trimpath -ldflags="-w -s" -o /out/kai ./kai-cli/cmd/kai

FROM --platform=linux/amd64 alpine:3.19
# Tools the review workflow invokes: git (checkout), jq (JSON assembly),
# curl (ingest POST), bash (the run: script), ca-certificates (HTTPS to kailab).
# libgcc: the CGO binary links against it at runtime.
RUN apk add --no-cache git jq curl bash ca-certificates libgcc
COPY --from=builder /out/kai /usr/local/bin/kai
WORKDIR /workspace
ENTRYPOINT []
CMD ["/bin/bash"]
