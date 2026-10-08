.PHONY: build test check integration menu

build:
	@bash scripts/version.sh >/dev/null
	@mkdir -p bin
	CGO_ENABLED=1 go build -trimpath -o bin/dozor ./cmd/dozor

test:
	go test -race ./...
	node --test ops/50-dozor_test.cjs
	node --test internal/dozor/live_player_test.cjs
	node --test internal/dozor/availability_test.cjs

check:
	go vet ./...
	test -z "$$(gofmt -l version*.go cmd internal ops)"
	node --check internal/dozor/web/app.js
	node --check internal/dozor/web/storage-policy.js
	node --check internal/dozor/web/live.js
	node --check internal/dozor/web/availability.js
	node --check internal/dozor/web/vendor/a00-menu.js
	bash -n scripts/install.sh scripts/build-release.sh scripts/version.sh
	bash scripts/version.sh
	PYTHONPYCACHEPREFIX="$(CURDIR)/.cache/pycache" python3 -m py_compile scripts/collect-licenses.py scripts/soak.py

integration: build
	DOZOR_INTEGRATION=1 DOZOR_BINARY="$(CURDIR)/bin/dozor" DOZOR_MEDIAMTX="$${DOZOR_MEDIAMTX:-$(CURDIR)/bin/mediamtx}" go test ./internal/dozor -run '^Test(RTSPPipeline|RTSPDisconnect|RTSPConnectionDiagnostics|LiveRTSPStream)$$' -v -count=1 -timeout=5m

# Rebuilds the shared Sidebar of @jourloy/00 into the committed bundle internal/dozor/web/vendor/a00-menu.{js,css}.
# Needs Node 20.9+ and the monorepo-frontend checkout (A00_DIR, default ../monorepo-frontend/packages/00).
# build and test do not depend on it: the outputs are committed, like hls.js.
menu:
	npm --prefix assets-src/menu ci
	node assets-src/menu/build.mjs
