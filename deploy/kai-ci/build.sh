#!/usr/bin/env bash
# Build and push the kai-ci toolbox image (the container the review workflow and
# the Cloud Agent workflow run in). Must be run from a workspace root holding
# kai-cli/, kai-core/, kai-engine/ and kai-tui/ side by side. Registry creds come from the kailab-registry-
# credentials k8s secret (same registry as kailab-control/kailab-runner).
#
# Builds linux/amd64 (the cluster arch). The builder runs natively and
# cross-compiles, so this is reliable on an arm64 Mac (no qemu compile).
#
#   ./kai-cli/deploy/kai-ci/build.sh            # build + push :latest
#   PUSH=0 ./kai-cli/deploy/kai-ci/build.sh     # build + load locally, no push
set -euo pipefail

# The registry PRODUCTION ACTUALLY PULLS FROM. The review workflow pins
# us-central1-docker.pkg.dev/kaicontext/kai/kai-ci by digest
# (kai-server .../review_default_workflow.go, defaultReviewImage), so a push to
# anywhere else produces an image the cluster cannot pull. This default was
# registry.kaicontext.com, which no longer matches that pin.
REGISTRY="${REGISTRY:-us-central1-docker.pkg.dev}"
IMAGE="${IMAGE:-$REGISTRY/kaicontext/kai/kai-ci}"
TAG="${TAG:-latest}"
PLATFORM="${PLATFORM:-linux/amd64}"
PUSH="${PUSH:-1}"
KUBECTL_CONTEXT="${KUBECTL_CONTEXT:-calendardev}"

# Resolve the workspace root (dir that contains all three module dirs).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
for m in kai-cli kai-core kai-engine kai-tui; do
  [ -d "$ROOT/$m" ] || { echo "missing module $ROOT/$m (run from the workspace root)"; exit 1; }
done

# The Dockerfile rewrites the pinned module versions to point at these
# checkouts, so the image contains whatever is checked out here — not what
# go.mod names. Print it, and refuse a dirty tree unless the operator insists:
# a build from uncommitted work is unreproducible, and one from a feature branch
# silently ships unreleased code under a released version number.
echo "Module refs going into this image:"
DIRTY=0
for m in kai-cli kai-core kai-engine kai-tui; do
  BRANCH="$(git -C "$ROOT/$m" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')"
  SHA="$(git -C "$ROOT/$m" rev-parse --short HEAD 2>/dev/null || echo '?')"
  STATE=""
  if [ -n "$(git -C "$ROOT/$m" status --porcelain 2>/dev/null)" ]; then STATE=" (UNCOMMITTED CHANGES)"; DIRTY=1; fi
  if [ "$BRANCH" != "main" ] && [ "$BRANCH" != "?" ]; then STATE="$STATE (not on main)"; DIRTY=1; fi
  printf '  %-11s %s @ %s%s\n' "$m" "$BRANCH" "$SHA" "$STATE"
done
if [ "$DIRTY" = "1" ] && [ "${ALLOW_DIRTY:-0}" != "1" ]; then
  echo
  echo "Refusing to build: at least one module is dirty or off main, so this image"
  echo "would not be reproducible. Commit/switch, or re-run with ALLOW_DIRTY=1."
  exit 1
fi

OUTPUT="--load"
if [ "$PUSH" = "1" ]; then
  case "$REGISTRY" in
    *.pkg.dev)
      # GCP Artifact Registry: an OAuth access token for an account with write
      # access to the kaicontext project. GCLOUD_ACCOUNT overrides which one.
      #
      # A stale refresh token fails here with "Reauthentication failed. cannot
      # prompt during non-interactive execution" — that needs `gcloud auth login`
      # in a real terminal; it cannot be done from a script.
      ACCOUNT="${GCLOUD_ACCOUNT:-fatih@kaicontext.com}"
      echo "Logging in to $REGISTRY as ${ACCOUNT}"
      TOKEN="$(env -u GOOGLE_APPLICATION_CREDENTIALS -u CLOUDSDK_CORE_ACCOUNT -u CLOUDSDK_CORE_PROJECT \
        gcloud auth print-access-token --account="$ACCOUNT")" || {
        echo "Could not get a token for $ACCOUNT. Run: gcloud auth login $ACCOUNT"
        exit 1
      }
      printf %s "$TOKEN" | docker login -u oauth2accesstoken --password-stdin "https://$REGISTRY"
      ;;
    *)
      echo "Logging in to $REGISTRY (creds from kailab-registry-credentials)…"
      U="$(kubectl --context "$KUBECTL_CONTEXT" -n kailab get secret kailab-registry-credentials -o jsonpath='{.data.username}' | base64 -d)"
      P="$(kubectl --context "$KUBECTL_CONTEXT" -n kailab get secret kailab-registry-credentials -o jsonpath='{.data.password}' | base64 -d)"
      printf %s "$P" | docker login "$REGISTRY" -u "$U" --password-stdin
      ;;
  esac
  OUTPUT="--push"
fi

echo "Building $IMAGE:$TAG ($PLATFORM) from $ROOT …"
docker buildx build \
  --platform "$PLATFORM" \
  -f "$ROOT/kai-cli/deploy/kai-ci/Dockerfile" \
  -t "$IMAGE:$TAG" \
  $OUTPUT \
  "$ROOT"

if [ "$PUSH" = "1" ]; then
  echo "Done: pushed $IMAGE:$TAG"
  # The digest is what gets pinned in code — a tag is not a pin.
  # The index digest, which is what a multi-arch image is pinned by. Parsed from
  # the human output: the --format template for this varies across buildx
  # versions and silently yields the Name line on a mismatch.
  DIGEST="$(docker buildx imagetools inspect "$IMAGE:$TAG" 2>/dev/null | awk '/^Digest:/{print $2; exit}')"
  [ -n "$DIGEST" ] && echo "Pin this: $IMAGE@$DIGEST"
else
  echo "Done: built + loaded $IMAGE:$TAG (not pushed)"
fi
