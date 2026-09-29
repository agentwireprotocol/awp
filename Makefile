VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
LDFLAGS   := -s -w
PLUGIN    := plugin/awp

.PHONY: build web test test-go test-python interop conformance schema dist plugin plugin-local clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/awp ./cmd/awp

# The web dashboard (awp web): built from web/ with bun, then embedded in
# the binary from internal/web/dist. Without it, awp web serves the API
# only.
web:
	cd web && bun install --frozen-lockfile && bun run build
	find internal/web/dist -mindepth 1 ! -name README -exec rm -rf {} +
	cp -R web/dist/. internal/web/dist/

test: test-go test-python interop conformance

test-go:
	go vet ./...
	go test -race ./...

test-python:
	cd python && python3 -m unittest test_awp_peer

interop: build
	cd python && AWP_BIN=$(CURDIR)/bin/awp python3 -m unittest -v interop_test

# The conformance suite (awp conform) against the Python peer, both ways.
# The Go node runs it in its own tests (internal/conformance).
conformance: build
	AWP_BIN=$(CURDIR)/bin/awp scripts/conformance.sh

# The JSON Schema and its reference page, schema/v0/, from the wire
# package. A test fails when they are stale.
schema:
	go generate ./wire

# Release artifacts in dist/: per-platform archives, the plugin bundle and
# SHA256SUMS. Tagging vX.Y.Z runs the same script in CI and publishes them.
dist:
	scripts/dist.sh $(VERSION)

# The agent plugin, with binaries for every supported platform.
plugin:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $(PLUGIN)/libexec/awp-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(PLUGIN)/libexec/awp-$$os-$$arch ./cmd/awp || exit 1; \
	done
	claude plugin validate $(PLUGIN) 2>/dev/null || true

# Just this machine's platform, for quick local testing.
plugin-local:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(PLUGIN)/libexec/awp-$$(go env GOOS)-$$(go env GOARCH) ./cmd/awp

clean:
	rm -rf bin dist web/dist $(PLUGIN)/libexec/awp-*
	find internal/web/dist -mindepth 1 ! -name README -exec rm -rf {} +
