#!/bin/bash
# Test script for workspace push functionality
# Tests that workspaces with UUID-based IDs can be pushed to remote servers

set -e  # Exit on error

TEST_DIR="/tmp/alice-test"
REMOTE_URL="${REMOTE_URL:-http://localhost:5173/kailab/alice}"

echo "=== Workspace Push Test ==="
echo "Test directory: $TEST_DIR"
echo "Remote URL: $REMOTE_URL"
echo ""

# Clean up and create test directory
rm -rf "$TEST_DIR"
mkdir -p "$TEST_DIR"
cd "$TEST_DIR"

echo "1. Initializing kai repository..."
kai init

echo "2. Creating source files..."
mkdir -p src
echo 'export const X = 1' > src/a.ts
echo 'export function f() { return X + 1 }' > src/b.ts

echo "3. Creating initial snapshot..."
kai snapshot --dir .

echo "4. Analyzing symbols..."
kai analyze symbols @snap:last

echo "5. Setting snap.main ref..."
kai ref set snap.main @snap:last

echo "6. Creating workspace feat/init..."
kai ws create --name feat/init --base snap.main

echo "7. Modifying source files..."
echo 'export const X = 2' > src/a.ts
echo 'export function f() { return X + 2 }' > src/b.ts

echo "8. Staging changes to workspace..."
kai ws stage --ws feat/init --dir .

echo "9. Rendering intent..."
# Intent is now stored as a separate node linked via HAS_INTENT edge
# to preserve content-addressing of ChangeSet nodes
kai intent render @cs:last --edit "Initialize project with basic math functions"

echo "10. Setting up remote..."
kai remote set origin "$REMOTE_URL"

echo "11. Pushing workspace to remote..."
kai push origin --ws feat/init

echo ""
echo "=== Test completed successfully! ==="
echo ""
echo "Workspace 'feat/init' has been pushed to $REMOTE_URL"
echo ""
echo "To verify, you can:"
echo "  1. Check the remote refs: kai fetch origin --list"
echo "  2. Fetch the workspace on another machine: kai fetch origin --ws feat/init"
