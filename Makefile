BINARY := filebadara
# The release workflow passes the tag; a local build labels itself from git.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DIST := dist
COVERPROFILE := coverage.out
COVERHTML := coverage.html
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

BUILD := go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)"

# Extra flags for every test target. `make test TESTFLAGS=-v` names each test and
# subtest with its own result. A cached run replays that output, so add
# -count=1 only when the tests need to genuinely run again.
TESTFLAGS ?=

.PHONY: all build test test-race test-helpers test-shell-helper \
	test-powershell-helper cover cover-html vet fmt dist clean

all: test build

build:
	$(BUILD) -o $(BINARY) ./cmd/filebadara

test:
	go test $(TESTFLAGS) ./...

# Kept separate from `test` because -race needs cgo and a C compiler, which a
# plain Go install does not have. CI runs this one.
test-race:
	go test -race $(TESTFLAGS) ./...

# The end-to-end tests that execute the served helper scripts, skipping whichever
# interpreter is missing. Set FILEBADARA_REQUIRE_HELPERS=1 to turn a missing
# interpreter into a failure.
test-helpers:
	go test -count=1 $(TESTFLAGS) -run 'HelperEndToEnd' ./internal/...

# Split out because each helper needs a different interpreter, and CI asserts
# each one on the platform that actually has it.
test-shell-helper:
	go test -count=1 $(TESTFLAGS) -run TestShellHelperEndToEnd ./internal/...

test-powershell-helper:
	go test -count=1 $(TESTFLAGS) -run TestPowerShellHelperEndToEnd ./internal/...

# Per-function coverage. atomic mode because several tests drive the server from
# more than one goroutine, which the cheaper set mode cannot count safely.
cover:
	go test -covermode=atomic -coverprofile=$(COVERPROFILE) $(TESTFLAGS) ./...
	go tool cover -func=$(COVERPROFILE)

# Same profile rendered as annotated source, showing which lines never ran.
cover-html: cover
	go tool cover -html=$(COVERPROFILE) -o $(COVERHTML)
	@echo "wrote $(COVERHTML)"

vet:
	go vet ./...

fmt:
	gofmt -w ./cmd ./internal

# Cross-compiled binaries plus checksums, ready to attach to a release.
# Uploaded as plain binaries rather than archives, so a download is one curl
# away. Statically linked, so they do not depend on the builder's libc.
dist:
	rm -rf $(DIST)
	mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(BUILD) \
			-o $(DIST)/$(BINARY)_$(VERSION)_$${os}_$${arch} ./cmd/filebadara || exit 1; \
	done
	cd $(DIST) && sha256sum $(BINARY)_* > SHA256SUMS
	@cat $(DIST)/SHA256SUMS

clean:
	rm -rf $(BINARY) $(DIST) $(COVERPROFILE) $(COVERHTML)
