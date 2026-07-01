#!/usr/bin/env bash
# Build and push the kai-ci toolbox image (the container the review workflow
# runs in). Must be run from a workspace root holding kai-cli/, kai-core/, and
# kai-engine/ side by side. Registry creds come from the kailab-registry-
# credentials k8s secret (same registry as kailab-control/kailab-runner).
#
#   ./kai-cli/deploy/kai-ci/build.sh            # build + push :latest and :<date>
#   PUSH=0 ./kai-cli/deploy/kai-ci/build.sh     # build only, no push
set -euo pipefail

REGISTRY="${REGISTRY:-registry.kaicontext.com}"
IMAGE="${IMAGE:-$REGISTRY/kai-ci}"
TAG="${TAG:-latest}"
PUSH="${PUSH:-1}"
KUBECTL_CONTEXT="${KUBECTL_CONTEXT:-calendardev}"

# Resolve the workspace root (dir that contains all three module dirs).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
for m in kai-cli kai-core kai-engine; do
  [ -d "$ROOT/$m" ] || { echo "missing module $ROOT/$m (run from the workspace root)"; exit 1; }
done

if [ "$PUSH" = "1" ]; then
  echo "Logging in to $REGISTRY (creds from kailab-registry-credentials)…"
  U="$(kubectl --context "$KUBECTL_CONTEXT" -n kailab get secret kailab-registry-credentials -o jsonpath='{.data.username}' | base64 -d)"
  P="$(kubectl --context "$KUBECTL_CONTEXT" -n kailab get secret kailab-registry-credentials -o jsonpath='{.data.password}' | base64 -d)"
  printf %s "$P" | docker login "$REGISTRY" -u "$U" --password-stdin
fi

echo "Building $IMAGE:$TAG from $ROOT …"
docker build \
  --platform linux/amd64 \
  -f "$ROOT/kai-cli/deploy/kai-ci/Dockerfile" \
  -t "$IMAGE:$TAG" \
  "$ROOT"

if [ "$PUSH" = "1" ]; then
  docker push "$IMAGE:$TAG"
  echo "Pushed $IMAGE:$TAG"
fi
