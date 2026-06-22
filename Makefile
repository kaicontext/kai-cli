.PHONY: build install test fmt clean bench demo

# macOS code-signing context (2026-05-14):
#
# Two failure modes worth knowing, both manifesting as SIGKILL:
#
# 1) Load-time rejection. Go's linker emits an "adhoc, linker-signed"
#    signature. On modern macOS (Sonoma 14.4+, Sequoia),
#    AppleSystemPolicy intermittently rejects linker-signed binaries
#    when com.apple.provenance is attached. Fix: re-sign with
#    `codesign --force --sign -` to replace the linker signature
#    with a proper ad-hoc one. This is what _sign below does.
#
# 2) Page-integrity rejection of a RUNNING process. `cp src dst`
#    truncates dst in place — the destination inode keeps the same
#    number but its bytes change. A still-running kai process holds
#    mmap'd pages from that inode; when the kernel pages one back
#    in and the page hash no longer matches the signature, it sends
#    SIGKILL. Fix: replace cp with atomic rename (install /
#    mv-from-tmp) so the running process keeps its old inode while
#    the new binary takes the path. The install_atomic target
#    handles project-local copies; ~/go/bin/kai is already atomic
#    because `go install` writes to a temp file and renames.

KAI_BIN := kai
GO_BIN := $(shell go env GOBIN)
ifeq ($(GO_BIN),)
	GO_BIN := $(shell go env GOPATH)/bin
endif

build:
	CGO_ENABLED=1 go build -o $(KAI_BIN) ./cmd/kai
	@$(MAKE) -s _sign FILE=$(KAI_BIN)

install:
	CGO_ENABLED=1 go install ./cmd/kai
	@$(MAKE) -s _sign FILE=$(GO_BIN)/kai
	@$(MAKE) -s _mirror

# _mirror copies the freshly-signed kai into the project-local path
# the user's PATH resolves. Uses `install` (BSD) which does an atomic
# create+rename — replacing in place would truncate the destination
# inode and SIGKILL any running kai that holds mmap'd pages from it.
# -m 755 keeps the executable bit. (The old unversioned
# /Users/jacobschatz/projects/kai/kai-cli tree was removed; this repo
# at kai/kai-cli is canonical.)
_mirror:
	@for dst in \
		/Users/jacobschatz/projects/kai/kai/kai-cli/kai; do \
		if [ -d "$$(dirname $$dst)" ]; then \
			install -m 755 $(GO_BIN)/kai "$$dst"; \
			codesign --force --sign - "$$dst" 2>/dev/null | grep -v 'replacing' || true; \
			echo "mirrored + signed: $$dst"; \
		fi; \
	done

# _sign is the codesign step factored out so build/install share it.
# Underscore-prefixed to discourage direct invocation; use resign for that.
_sign:
	@if [ -z "$(FILE)" ]; then echo "_sign: FILE is required"; exit 1; fi
	@codesign --force --sign - "$(FILE)" 2>&1 | grep -v 'replacing existing signature' || true
	@echo "signed: $(FILE)"

# resign is the manual recovery target when a binary was built outside
# this Makefile (e.g. a raw `go install`) and needs to be re-signed
# before macOS will run it cleanly.
resign:
	@codesign --force --sign - $(GO_BIN)/kai
	@echo "resigned $(GO_BIN)/kai"

test:
	CGO_ENABLED=1 go test ./...

# check-tui-imports reports which engine packages the TUI still
# imports directly. Long-term target is zero — everything should
# pass through kai-cli/api/. Currently informational only; flip to
# --strict once the migration is done. See:
#   docs/architecture/tui-api-extraction.md
check-tui-imports:
	@bash ../scripts/check-tui-imports.sh

fmt:
	gofmt -s -w .

clean:
	rm -f kai
	rm -rf testdata/repo/.kai

bench: build
	../bench/run.sh -k ./kai --skip-build

# Run the demo workflow
demo: build
	@echo "=== Initializing Kai in testdata/repo ==="
	cd testdata/repo && ../../kai init
	@echo ""
	@echo "=== Creating snapshot of main branch ==="
	cd testdata/repo && ../../kai snapshot main --repo .
	@echo ""
	@echo "=== Creating snapshot of feature branch ==="
	cd testdata/repo && ../../kai snapshot feature --repo .
	@echo ""
	@echo "=== Demo complete! Use the snapshot IDs above to analyze symbols and create changesets ==="
