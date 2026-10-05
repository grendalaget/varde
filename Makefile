GO_MODULES := go apps/control-plane apps/relay apps/mesh
BIN := bin

export PATH := $(HOME)/go/bin:$(PATH)

.PHONY: gen build test lint fmt check-gen clean

gen:
	buf generate
	pnpm --dir apps/web gen:api

build:
	@for m in $(GO_MODULES); do (cd $$m && go build ./...) || exit 1; done
	cargo build --workspace
	pnpm --dir apps/web build
	@mkdir -p $(BIN)
	@for m in control-plane relay mesh; do \
		(cd apps/$$m && go build -o ../../$(BIN)/p2pgames-$$m .) || exit 1; \
	done
	cp target/debug/p2pgames-agent $(BIN)/p2pgames-agent

test:
	@for m in $(GO_MODULES); do (cd $$m && go test ./...) || exit 1; done
	cargo test --workspace

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

check-gen: gen
	@git diff --exit-code -- go/gen apps/web/src/api/schema.d.ts \
		|| (echo "generated files are stale; run 'make gen' and commit" && exit 1)

clean:
	rm -rf $(BIN) target apps/web/dist
