MODULES := . harness agenui-agent

.PHONY: init dev verify build test vet test-runtime test-harness test-agenui web-build diagrams clean

init:
	cd agenui-agent && go run ./cmd/agenui-local-demo -sqlite var/agenui/studio.db -init-only

dev: init
	@set -e; \
	(cd agenui-agent && go run ./cmd/agenui-local-demo -sqlite var/agenui/studio.db -listen 127.0.0.1:18082) & demo_pid=$$!; \
	(cd agenui-agent && AGENUI_DATABASE_DSN='file:var/agenui/studio.db' go run ./cmd/agenui-agent -config configs/environments/local/agenui.toml) & agent_pid=$$!; \
	(cd web && npm run dev) & web_pid=$$!; \
	trap 'kill $$demo_pid $$agent_pid $$web_pid 2>/dev/null || true' INT TERM EXIT; \
	wait

# AGenUI Studio release modules.
verify: build test vet web-build

build:
	@for m in $(MODULES); do (cd $$m && go build ./...) || exit 1; done

test: test-runtime test-harness test-agenui

test-runtime:
	go test ./runtime/...

test-harness:
	cd harness && go test ./...

test-agenui:
	cd agenui-agent && go test ./...

vet:
	go vet ./...
	cd harness && go vet ./...
	cd agenui-agent && go vet ./...

web-build:
	cd packages/renderer && npm test && npm run build
	cd web && npm test && npm run build

diagrams:
	dot -Tsvg docs/diagrams/architecture.dot -o docs/assets/architecture.svg
	dot -Tsvg docs/diagrams/generation-loop.dot -o docs/assets/generation-loop.svg
	node docs/diagrams/render-product-overview.mjs

clean:
	rm -rf agenui-agent/var harness/var web/.next web/.next-build
