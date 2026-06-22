#!/usr/bin/env bash
# Check and optionally add SPDX copyright headers to source files.
# Usage:
#   ./scripts/check-copyright-headers.sh          # Check only (exit 1 if missing)
#   ./scripts/check-copyright-headers.sh --fix     # Add missing headers

set -euo pipefail

FIX=false
if [[ "${1:-}" == "--fix" ]]; then
    FIX=true
fi

HEADER_GO="// Copyright 2024-2026 Kai Authors
// SPDX-License-Identifier: Apache-2.0
"

HEADER_SH="# Copyright 2024-2026 Kai Authors
# SPDX-License-Identifier: Apache-2.0
"

HEADER_TS="// Copyright 2024-2026 Kai Authors
// SPDX-License-Identifier: Apache-2.0
"

MISSING=0

check_file() {
    local file="$1"
    local header_pattern="SPDX-License-Identifier: Apache-2.0"

    if ! head -5 "$file" | grep -q "$header_pattern"; then
        if $FIX; then
            local ext="${file##*.}"
            local tmpfile
            tmpfile=$(mktemp)

            case "$ext" in
                go)
                    printf '%s\n' "$HEADER_GO" | cat - "$file" > "$tmpfile"
                    ;;
                sh)
                    # Preserve shebang line
                    if head -1 "$file" | grep -q '^#!'; then
                        head -1 "$file" > "$tmpfile"
                        echo "" >> "$tmpfile"
                        printf '%s\n' "$HEADER_SH" >> "$tmpfile"
                        tail -n +2 "$file" >> "$tmpfile"
                    else
                        printf '%s\n' "$HEADER_SH" | cat - "$file" > "$tmpfile"
                    fi
                    ;;
                ts|js|svelte)
                    printf '%s\n' "$HEADER_TS" | cat - "$file" > "$tmpfile"
                    ;;
                *)
                    rm "$tmpfile"
                    return
                    ;;
            esac

            mv "$tmpfile" "$file"
            echo "  FIXED: $file"
        else
            echo "  MISSING: $file"
            MISSING=$((MISSING + 1))
        fi
    fi
}

# Find source files, excluding vendor/node_modules/testdata/generated
find_sources() {
    find kai-core kai-cli \
        -type f \( -name '*.go' -o -name '*.sh' \) \
        ! -path '*/vendor/*' \
        ! -path '*/node_modules/*' \
        ! -path '*/testdata/*' \
        ! -path '*_test.go' \
        2>/dev/null | sort
}

echo "Checking copyright headers..."
while IFS= read -r file; do
    check_file "$file"
done < <(find_sources)

if ! $FIX && [[ $MISSING -gt 0 ]]; then
    echo ""
    echo "$MISSING files missing SPDX headers. Run with --fix to add them."
    exit 1
fi

echo "Done."
