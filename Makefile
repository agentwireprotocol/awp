VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
LDFLAGS   := -s -w
PLUGIN    := plugin/awp

.PHONY: build web test test-go carriers schema dist plugin plugin-local clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/awp ./cmd/awp

# The web dashboard (awp web): built from web/ with bun, then embedded in
# the binary from internal/web/dist. Without it, awp web serves the API
# only.
web:
	cd web && bun install --frozen-lockfile && bun run build
	find internal/web/dist -mindepth 1 ! -name README -exec rm -rf {} +
	cp -R web/dist/. internal/web/dist/

test: test-go

# The Go tests include the conformance suite against the Go node, both
# ways. The SDKs run it against themselves in their own repositories.
test-go:
	go vet ./...
	go test -race ./...

# The carriers that need the internet: tailcat through its DERP relays, and
# a WebSocket through a Cloudflare quick tunnel (needs cloudflared).
carriers:
	AWP_TEST_TAILCAT=1 AWP_TEST_CLOUDFLARE=1 go test -count=1 -v -run 'TestTailcat|TestCloudflare' ./tunnel

# The JSON Schema and its reference page, schema/v1/, from the wire
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
