GO_MODULES := go apps/control-plane apps/relay apps/mesh tests/e2e
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
	@for m in control-plane relay mesh; do \
		(cd apps/$$m && go build -o ../../$(BIN)/p2pgames-$$m .) || exit 1; \
	done
	cp target/debug/p2pgames-agent $(BIN)/p2pgames-agent

# Copy the built SPA into the Go module so go:embed picks it up. dist/ is
# fully untracked so `git status` stays clean; without a web build we
# materialize it from the committed placeholder instead.
.PHONY: webui-dist
webui-dist:
	@rm -rf apps/control-plane/internal/webui/dist
	@if [ -d apps/web/dist ]; then \
		cp -r apps/web/dist apps/control-plane/internal/webui/dist; \
	else \
		mkdir -p apps/control-plane/internal/webui/dist && \
		cp apps/control-plane/internal/webui/placeholder.html \
			apps/control-plane/internal/webui/dist/index.html; \
	fi

test: test-bins
	@for m in $(GO_MODULES); do (cd $$m && go test ./...) || exit 1; done
	cargo test --workspace

# e2e tests execute the real binaries; build them first (idempotent).
.PHONY: test-bins
test-bins:
	$(MAKE) webui-dist
	@mkdir -p $(BIN)
	@for m in control-plane relay mesh; do \
		(cd apps/$$m && go build -o ../../$(BIN)/p2pgames-$$m .) || exit 1; \
	done
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

check-gen: gen gen-go-api
	@git diff --exit-code -- go/gen apps/web/src/api/schema.d.ts \
		apps/control-plane/internal/api/gen \
		|| (echo "generated files are stale; run 'make gen' and commit" && exit 1)

.PHONY: gen-go-api
gen-go-api:
	cd api/openapi && oapi-codegen -config oapi-codegen.yaml control-plane.yaml

clean:
	rm -rf $(BIN) target apps/web/dist
