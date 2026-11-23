# akey build. All go commands use the local toolchain (no auto-download).
GO        := GOTOOLCHAIN=local go
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || date -u +%Y%m%d-%H%M)
LDFLAGS   := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: build test dist clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/akey ./cmd/akey

test:
	$(GO) test ./...

dist:
	rm -rf dist && mkdir -p dist
	@set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; \
		if [ "$$os" = windows ]; then ext=.exe; fi; \
		echo "build dist/akey-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/akey-$$os-$$arch$$ext ./cmd/akey; \
	done
	cd dist && if command -v sha256sum >/dev/null 2>&1; then sha256sum akey-*; else shasum -a 256 akey-*; fi > SHA256SUMS
	@cat dist/SHA256SUMS

clean:
	rm -rf bin dist
