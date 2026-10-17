# holocron. `make help` lists the targets; SPEC.md § 11 is the build and
# release design. Releases are built by GitHub Actions from a tag
# (.github/workflows/release.yml), never on this machine.

VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo dev))
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64
DIST := dist

.DEFAULT_GOAL := help
.PHONY: help build run test fmt check dist clean release

help: ## This list
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-8s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build for this machine: bin/holocron
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/holocron ./cmd/holocron

run: build ## Run the server here: make run ARGS='--listen 127.0.0.1:8080'
	./bin/holocron serve $(ARGS)

test: ## Unit tests (offline)
	go test ./...

fmt: ## Format the Go sources in place
	gofmt -w .

check: ## Before every commit: gofmt, go vet, go mod verify, go test
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed (make fmt):"; echo "$$out"; exit 1; fi
	go vet ./...
	go mod verify
	go test ./...

dist: ## Static Linux binaries and SHA256SUMS in dist/, as a release publishes them
	rm -rf $(DIST) && mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  echo "$$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
	    -o $(DIST)/holocron-$$os-$$arch ./cmd/holocron || exit 1; \
	done
	cd $(DIST) && shasum -a 256 holocron-* > SHA256SUMS && cat SHA256SUMS

clean: ## Remove bin/ and dist/
	rm -rf bin $(DIST)

release: ## Tag vX.Y.Z on a pushed, clean main; GitHub builds and publishes: make release V=X.Y.Z
	@printf '%s' "$(V)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "usage: make release V=X.Y.Z (got '$(V)')" >&2; exit 1; }
	@[ "$$(git branch --show-current)" = main ] || { echo "release: not on main" >&2; exit 1; }
	@[ -z "$$(git status --porcelain)" ] || { echo "release: the tree is not clean" >&2; exit 1; }
	@git fetch -q origin main && [ "$$(git rev-parse HEAD)" = "$$(git rev-parse origin/main)" ] || { echo "release: main differs from origin/main; push or pull first" >&2; exit 1; }
	@! git rev-parse -q --verify "refs/tags/v$(V)" >/dev/null || { echo "release: v$(V) already exists" >&2; exit 1; }
	$(MAKE) check
	git tag -a "v$(V)" -m "holocron v$(V)"
	git push origin "v$(V)"
	@echo "release: v$(V) pushed; the release workflow builds and publishes it"
