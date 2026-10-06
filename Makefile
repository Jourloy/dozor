.PHONY: build test check integration

build:
	@mkdir -p bin
	CGO_ENABLED=1 go build -trimpath -o bin/dozor ./cmd/dozor

test:
	go test -race ./...
	node --test ops/50-dozor_test.cjs
	node --test internal/dozor/live_player_test.cjs

check:
	go vet ./...
	test -z "$$(gofmt -l cmd internal)"
	node --check internal/dozor/web/app.js
	node --check internal/dozor/web/live.js
	bash -n scripts/install.sh scripts/build-release.sh
	PYTHONPYCACHEPREFIX="$(CURDIR)/.cache/pycache" python3 -m py_compile scripts/collect-licenses.py scripts/soak.py

integration: build
	DOZOR_INTEGRATION=1 DOZOR_BINARY="$(CURDIR)/bin/dozor" DOZOR_MEDIAMTX="$${DOZOR_MEDIAMTX:-$(CURDIR)/bin/mediamtx}" go test ./internal/dozor -run '^Test(RTSPPipeline|LiveRTSPStream)$$' -v -count=1 -timeout=5m
