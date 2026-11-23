# akey build.
#
#   make            build bin/akey (with the web panel embedded)
#   make wasm       build the panel's WASM module into internal/server/web/
#   make test       go test ./...
#   make check      fmt-check + vet + test
#   make dist       cross-compile dist/akey-<os>-<arch>[.exe] + SHA256SUMS
#
# Go commands use the local toolchain by default (no auto-download);
# override with e.g. `make GOTOOLCHAIN=auto`.
GOTOOLCHAIN ?= local
export GOTOOLCHAIN

GO        ?= go
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || date -u +%Y%m%d-%H%M)
LDFLAGS   := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

WEB_DIR   := internal/server/web
WASM      := $(WEB_DIR)/akey.wasm
WASM_EXEC := $(WEB_DIR)/wasm_exec.js

.PHONY: build wasm test vet fmt fmt-check check dist clean

build: wasm
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/akey ./cmd/akey

# The server embeds internal/server/web, so build/dist need these outputs.
# wasm_exec.js must come from the same Go version that built akey.wasm
# (Go >= 1.24 keeps it in lib/wasm/, older releases in misc/wasm/).
wasm:
	CGO_ENABLED=0 GOOS=js GOARCH=wasm $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(WASM) ./cmd/akey-wasm
	@root="$$($(GO) env GOROOT)"; src="$$root/lib/wasm/wasm_exec.js"; \
	[ -f "$$src" ] || src="$$root/misc/wasm/wasm_exec.js"; \
	[ -f "$$src" ] || { echo "wasm_exec.js not found under $$root" >&2; exit 1; }; \
	cp "$$src" $(WASM_EXEC) && echo "cp $$src $(WASM_EXEC)"

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...
	GOOS=js GOARCH=wasm $(GO) vet ./cmd/akey-wasm

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:" >&2; echo "$$out" >&2; exit 1; fi

check: fmt-check vet test

# Asset names are a contract: the server's /dl/ whitelist, install.sh,
# install.ps1 and scripts/install-server.sh all expect exactly these six
# binaries plus SHA256SUMS (which lists only the binaries).
dist: wasm
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
	rm -rf bin dist $(WASM) $(WASM_EXEC)
