BINARY := filebadara
# The release workflow passes the tag; a local build labels itself from git.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DIST := dist
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

BUILD := go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)"

.PHONY: all build test test-race vet fmt dist clean

all: test build

build:
	$(BUILD) -o $(BINARY) ./cmd/filebadara

test:
	go test ./...

# Kept separate from `test` because -race needs cgo and a C compiler, which a
# plain Go install does not have. CI runs this one.
test-race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w ./cmd ./internal

# Cross-compiled archives plus checksums, ready to attach to a release.
# Statically linked so the binary does not depend on the builder's libc.
dist:
	rm -rf $(DIST)
	mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(BUILD) -o $(DIST)/$(BINARY) ./cmd/filebadara || exit 1; \
		tar czf $(DIST)/$(BINARY)_$(VERSION)_$${os}_$${arch}.tar.gz -C $(DIST) $(BINARY) -C .. LICENSE README.md || exit 1; \
		rm $(DIST)/$(BINARY); \
	done
	cd $(DIST) && sha256sum *.tar.gz > SHA256SUMS
	@cat $(DIST)/SHA256SUMS

clean:
	rm -rf $(BINARY) $(DIST)
