#!/bin/bash
# End-to-end test for kai test affected command
# Tests that the import graph tracing correctly identifies affected tests

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m' # No Color

# Create temp directory
TESTDIR=$(mktemp -d)
trap "rm -rf $TESTDIR" EXIT

echo "=== E2E Test: kai test affected ==="
echo "Test directory: $TESTDIR"

# Build kai
echo -n "Building kai... "
go build -o "$TESTDIR/kai" ./cmd/kai
echo "done"

cd "$TESTDIR"

# Create project structure
mkdir -p src/utils src/components

# Create source files with import chain:
# Button.test.tsx -> Button.tsx -> format.ts -> math.ts
# math.test.ts -> math.ts

cat > src/utils/math.ts << 'EOF'
// Deep utility - no imports
export function add(a: number, b: number): number {
    return a + b;
}

export function multiply(a: number, b: number): number {
    return a * b;
}
EOF

cat > src/utils/format.ts << 'EOF'
// Imports math.ts
import { multiply } from './math';

export function formatCurrency(amount: number): string {
    return `$${multiply(amount, 100) / 100}`;
}
EOF

cat > src/components/Button.tsx << 'EOF'
// Imports format.ts (which imports math.ts)
import { formatCurrency } from '../utils/format';

export function Button({ price }: { price: number }) {
    return <button>{formatCurrency(price)}</button>;
}
EOF

cat > src/components/Button.test.tsx << 'EOF'
// Test file - imports Button (which imports format -> math)
import { Button } from './Button';

describe('Button', () => {
    it('renders price', () => {
        expect(Button({ price: 10 })).toBeDefined();
    });
});
EOF

cat > src/utils/math.test.ts << 'EOF'
// Direct test for math.ts
import { add, multiply } from './math';

describe('math', () => {
    it('adds numbers', () => {
        expect(add(1, 2)).toBe(3);
    });
});
EOF

cat > src/standalone.ts << 'EOF'
// Standalone file with no tests
export function standalone() {
    return 'no tests cover me';
}
EOF

# Initialize kai
echo -n "Initializing kai... "
./kai init > /dev/null
echo "done"

# Create first snapshot
echo -n "Creating baseline snapshot... "
./kai snapshot --dir . > /dev/null
echo "done"

# Analyze calls
echo -n "Analyzing calls (building import graph)... "
./kai analyze calls @snap:last > /dev/null
echo "done"

# Modify math.ts (deep dependency)
echo -n "Modifying src/utils/math.ts... "
cat > src/utils/math.ts << 'EOF'
// Deep utility - no imports - MODIFIED
export function add(a: number, b: number): number {
    return a + b + 0; // Changed!
}

export function multiply(a: number, b: number): number {
    return a * b;
}
EOF
echo "done"

# Create second snapshot
echo -n "Creating snapshot after change... "
./kai snapshot --dir . > /dev/null
echo "done"

# Analyze calls for new snapshot
echo -n "Analyzing calls for new snapshot... "
./kai analyze calls @snap:last > /dev/null
echo "done"

# Test 1: Changing math.ts should affect both test files
echo ""
echo "=== Test 1: Change to math.ts ==="
echo "Expected: Both Button.test.tsx and math.test.ts should be affected"
echo "(Button.test.tsx transitively imports math.ts via Button.tsx -> format.ts)"
echo ""

AFFECTED=$(./kai test affected @snap:prev @snap:last)
echo "$AFFECTED"
echo ""

# Verify results
if echo "$AFFECTED" | grep -q "src/components/Button.test.tsx" && \
   echo "$AFFECTED" | grep -q "src/utils/math.test.ts"; then
    echo -e "${GREEN}✓ Test 1 PASSED: Both test files detected${NC}"
else
    echo -e "${RED}✗ Test 1 FAILED: Expected both test files to be affected${NC}"
    exit 1
fi

# Test 2: Change standalone.ts (no test coverage)
echo ""
echo "=== Test 2: Change to standalone.ts (no tests) ==="

cat > src/standalone.ts << 'EOF'
// Standalone file with no tests - MODIFIED
export function standalone() {
    return 'still no tests';
}
EOF

./kai snapshot --dir . > /dev/null
./kai analyze calls @snap:last > /dev/null

AFFECTED2=$(./kai test affected @snap:prev @snap:last)
echo "$AFFECTED2"
echo ""

if echo "$AFFECTED2" | grep -q "No affected test files found"; then
    echo -e "${GREEN}✓ Test 2 PASSED: No tests affected for uncovered file${NC}"
else
    echo -e "${RED}✗ Test 2 FAILED: Expected no affected tests${NC}"
    exit 1
fi

# Test 3: Change only format.ts (middle of chain)
echo ""
echo "=== Test 3: Change to format.ts (middle of import chain) ==="

cat > src/utils/format.ts << 'EOF'
// Imports math.ts - MODIFIED
import { multiply } from './math';

export function formatCurrency(amount: number): string {
    return `$${multiply(amount, 100) / 100}`; // Changed formatting
}
EOF

./kai snapshot --dir . > /dev/null
./kai analyze calls @snap:last > /dev/null

AFFECTED3=$(./kai test affected @snap:prev @snap:last)
echo "$AFFECTED3"
echo ""

# format.ts is imported by Button.tsx, which is imported by Button.test.tsx
# math.test.ts does NOT import format.ts, so it should NOT be affected
if echo "$AFFECTED3" | grep -q "src/components/Button.test.tsx"; then
    echo -e "${GREEN}✓ Test 3 PASSED: Button.test.tsx affected by format.ts change${NC}"
else
    echo -e "${RED}✗ Test 3 FAILED: Expected Button.test.tsx to be affected${NC}"
    exit 1
fi

if echo "$AFFECTED3" | grep -q "math.test.ts"; then
    echo -e "${RED}✗ Test 3 FAILED: math.test.ts should NOT be affected by format.ts change${NC}"
    exit 1
else
    echo -e "${GREEN}✓ Test 3 PASSED: math.test.ts correctly not affected${NC}"
fi

# Test 4: CommonJS require() syntax
echo ""
echo "=== Test 4: CommonJS require() syntax ==="

# Create new CommonJS files
mkdir -p src/cjs

cat > src/cjs/data.js << 'EOF'
// CommonJS module
module.exports = {
    users: ['alice', 'bob']
};
EOF

cat > src/cjs/service.js << 'EOF'
// CommonJS - imports data.js
const data = require('./data');

function getUsers() {
    return data.users;
}

module.exports = { getUsers };
EOF

cat > src/cjs/service.test.js << 'EOF'
// CommonJS test - imports service.js which imports data.js
const { getUsers } = require('./service');

describe('service', () => {
    it('gets users', () => {
        expect(getUsers().length).toBe(2);
    });
});
EOF

./kai snapshot --dir . > /dev/null
./kai analyze calls @snap:last > /dev/null

# Now modify data.js (deep CommonJS dependency)
cat > src/cjs/data.js << 'EOF'
// CommonJS module - MODIFIED
module.exports = {
    users: ['alice', 'bob', 'charlie']
};
EOF

./kai snapshot --dir . > /dev/null
./kai analyze calls @snap:last > /dev/null

AFFECTED4=$(./kai test affected @snap:prev @snap:last)
echo "$AFFECTED4"
echo ""

if echo "$AFFECTED4" | grep -q "src/cjs/service.test.js"; then
    echo -e "${GREEN}✓ Test 4 PASSED: CommonJS test file detected via require() chain${NC}"
else
    echo -e "${RED}✗ Test 4 FAILED: Expected service.test.js to be affected by data.js change${NC}"
    exit 1
fi

echo ""
echo -e "${GREEN}=== All E2E tests passed ===${NC}"
