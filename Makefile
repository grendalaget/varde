GO_MODULES := go apps/controlplane apps/relay apps/mesh tests/e2e
BIN := bin

export PATH := $(HOME)/go/bin:$(PATH)

.PHONY: gen build test lint fmt check-gen clean

gen:
	buf generate
	pnpm --dir apps/web gen:api

build:
	cargo build --workspace
	pnpm --dir apps/web build
	$(MAKE) webui-dist
	@for m in $(GO_MODULES); do (cd $$m && go build ./...) || exit 1; done
	@mkdir -p $(BIN)
	(cd apps/controlplane && go build -o ../../$(BIN)/varde-control-plane .)
	(cd apps/relay && go build -o ../../$(BIN)/varde-relay .)
	(cd apps/mesh && go build -o ../../$(BIN)/varde-mesh .)
	cp target/debug/varde-agent $(BIN)/varde-agent

# Copy the built SPA into the Go module so go:embed picks it up. dist/ is
# fully untracked so `git status` stays clean; without a web build we
# materialize it from the committed placeholder instead.
.PHONY: webui-dist
webui-dist:
	@rm -rf apps/controlplane/internal/webui/dist
	@if [ -d apps/web/dist ]; then \
		cp -r apps/web/dist apps/controlplane/internal/webui/dist; \
	else \
		mkdir -p apps/controlplane/internal/webui/dist && \
		cp apps/controlplane/internal/webui/placeholder.html \
			apps/controlplane/internal/webui/dist/index.html; \
	fi

test: test-bins
	@for m in $(GO_MODULES); do (cd $$m && go test ./...) || exit 1; done
	cargo test --workspace

# e2e tests execute the real binaries; build them first (idempotent).
.PHONY: test-bins
test-bins:
	$(MAKE) webui-dist
	@mkdir -p $(BIN)
	(cd apps/controlplane && go build -o ../../$(BIN)/varde-control-plane .)
	(cd apps/relay && go build -o ../../$(BIN)/varde-relay .)
	(cd apps/mesh && go build -o ../../$(BIN)/varde-mesh .)
	cargo build --bins

lint:
	buf lint
	@for m in $(GO_MODULES); do (cd $$m && go vet ./... && golangci-lint run ./...) || exit 1; done
	cargo clippy --workspace -- -D warnings
	pnpm --dir apps/web lint
	pnpm --dir apps/web typecheck

fmt:
	@for m in $(GO_MODULES); do (cd $$m && gofmt -w .) || exit 1; done
	cargo fmt
	pnpm --dir apps/web format

# Linux packages (.deb + .rpm) via nfpm. Requires nfpm on PATH.
#   make package VERSION=0.1.0
.PHONY: package
package: VERSION ?= 0.0.0-dev
package: build
	@mkdir -p dist
	cp bin/varde-agent bin/varde-mesh bin/varde-control-plane bin/varde-relay dist/
	@for p in varde-agent varde-control-plane varde-relay; do \
		cfg=packaging/linux/nfpm.yaml; \
		[ $$p = varde-agent ] || cfg=packaging/linux/nfpm-$${p#varde-}.yaml; \
		for pack in deb rpm; do \
			ARCH=amd64 VERSION=$(VERSION) nfpm package \
				--config $$cfg --packager $$pack \
				--target dist/$$p-$(VERSION)-amd64.$$pack || exit 1; \
		done; \
	done

check-gen: gen gen-go-api
	@git diff --exit-code -- go/gen apps/web/src/api/schema.d.ts \
		apps/controlplane/internal/api/gen \
		|| (echo "generated files are stale; run 'make gen' and commit" && exit 1)

.PHONY: gen-go-api
gen-go-api:
	cd api/openapi && oapi-codegen -config oapi-codegen.yaml control-plane.yaml

clean:
	rm -rf $(BIN) target apps/web/dist

# Root-only Linux netns demo (networking.md §64): builds as the normal
# user, then runs only the test binary as root (sudo loses HOME/rustup).
.PHONY: e2e-netns
e2e-netns: test-bins
	@if ! sudo -n true 2>/dev/null; then echo "e2e-netns needs root: run 'sudo -v' first"; exit 1; fi
	cd tests/netns && go test -c -tags netns -o ../../bin/netns.test .
	cd tests/netns && sudo ../../bin/netns.test -test.v -test.count=1

# Minecraft e2e on the same netns topology (failover + owner shutdown).
# Needs root plus node (VARDE_NODE if sudo's PATH hides it).
.PHONY: e2e-minecraft
e2e-minecraft: test-bins
	@if ! sudo -n true 2>/dev/null; then echo "e2e-minecraft needs root: run 'sudo -v' first"; exit 1; fi
	pnpm --filter varde-minecraft-bot install
	cd tests/netns && go test -c -tags 'netns minecraft' -o ../../bin/netns-minecraft.test .
	cd tests/netns && sudo VARDE_NODE=$(shell command -v node) \
		VARDE_E2E_RUNTIME_SEED="$(VARDE_E2E_RUNTIME_SEED)" \
		../../bin/netns-minecraft.test -test.run TestMinecraft -test.v -test.count=1 -test.timeout 40m

# Randomized Minecraft fault soak. Set VARDE_SOAK_SEED to reproduce a run.
SOAK_MINUTES ?= 60
.PHONY: e2e-soak
e2e-soak: test-bins
	@if ! sudo -n true 2>/dev/null; then echo "e2e-soak needs root: run 'sudo -v' first"; exit 1; fi
	pnpm --filter varde-minecraft-bot install
	cd tests/netns && go test -c -tags 'netns minecraft' -o ../../bin/netns-minecraft.test .
	cd tests/netns && sudo VARDE_NODE=$(shell command -v node) \
		VARDE_E2E_RUNTIME_SEED="$(VARDE_E2E_RUNTIME_SEED)" \
		VARDE_SOAK_DURATION=$(SOAK_MINUTES)m VARDE_SOAK_SEED="$(VARDE_SOAK_SEED)" \
		../../bin/netns-minecraft.test -test.run '^TestMinecraftSoak$$' -test.v -test.count=1 \
		-test.timeout $$(expr $(SOAK_MINUTES) + 30)m

# Valheim real-server e2e on the same netns topology (failover + Crossplay).
.PHONY: e2e-valheim
e2e-valheim: test-bins
	@if ! sudo -n true 2>/dev/null; then echo "e2e-valheim needs root: run 'sudo -v' first"; exit 1; fi
	cd tests/netns && go test -c -tags 'netns valheim' -o ../../bin/netns-valheim.test .
	cd tests/netns && sudo VARDE_VALHEIM_SEED_DIR="$(VARDE_VALHEIM_SEED_DIR)" \
		VARDE_E2E_RUNTIME_SEED="$(VARDE_E2E_RUNTIME_SEED)" \
		../../bin/netns-valheim.test -test.run TestValheim -test.v -test.count=1 -test.timeout 75m
