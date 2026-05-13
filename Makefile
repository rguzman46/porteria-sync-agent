# Sync Agent — build pipeline.
#
# Targets:
#   make build          → binario para la plataforma actual (./bin/porteria-agent)
#   make windows        → cross-compile para Windows amd64 (./bin/porteria-agent.exe)
#   make linux          → cross-compile para Linux amd64
#   make macos          → cross-compile para macOS arm64 (Apple Silicon)
#   make all            → todos los anteriores
#   make clean          → limpia ./bin
#   make test           → unit tests (cuando existan)
#   make sha256         → calcula hash SHA-256 del binario Windows (para install.ps1)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.AgentVersion=$(VERSION)

.PHONY: build windows linux macos all clean test sha256

build:
	@mkdir -p bin
	go build -ldflags="$(LDFLAGS)" -o bin/porteria-agent .
	@echo "✓ Built ./bin/porteria-agent ($(VERSION))"

windows:
	@mkdir -p bin
	GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o bin/porteria-agent.exe .
	@echo "✓ Built ./bin/porteria-agent.exe ($(VERSION) — Windows amd64)"

linux:
	@mkdir -p bin
	GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o bin/porteria-agent-linux .
	@echo "✓ Built ./bin/porteria-agent-linux ($(VERSION))"

macos:
	@mkdir -p bin
	GOOS=darwin GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o bin/porteria-agent-macos .
	@echo "✓ Built ./bin/porteria-agent-macos ($(VERSION))"

all: windows linux macos
	@echo "✓ Built all platforms"

clean:
	rm -rf bin
	@echo "✓ ./bin/ limpiado"

test:
	go test -v ./...

sha256:
	@if [ ! -f bin/porteria-agent.exe ]; then \
		echo "✗ Run 'make windows' first"; exit 1; \
	fi
	@shasum -a 256 bin/porteria-agent.exe | awk '{print $$1}'
