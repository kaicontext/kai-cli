# Test Scripts

Manual E2E test scripts for verifying kai functionality.

## Scripts

### `test_affected_e2e.sh`

End-to-end test for the `kai test affected` command. Tests that the import graph tracing correctly identifies affected tests when source files change.

**What it tests:**
- Transitive dependency tracking (Button.test.tsx -> Button.tsx -> format.ts -> math.ts)
- Files with no test coverage
- Changes to middle of import chain
- CommonJS `require()` syntax

**Usage:**
```bash
./scripts/test_affected_e2e.sh
```

### `test-ws-push.sh`

Tests workspace push functionality. Verifies that workspaces with UUID-based IDs can be pushed to remote servers.

**What it tests:**
- Workspace creation and staging
- Intent rendering
- Remote push with proper UUID handling

**Usage:**
```bash
REMOTE_URL=http://localhost:5173/kailab/alice ./scripts/test-ws-push.sh
```

Requires a running kai server at the specified URL.
