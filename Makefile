# Builds Ares for Linux and Windows. Run `make help` for the targets.

BIN := bin

# MinGW-w64 posix compilers: the audio processing uses C++ threads.
WIN_CC  ?= x86_64-w64-mingw32-gcc-posix
WIN_CXX ?= x86_64-w64-mingw32-g++-posix

# The WebRTC APM needs a few tweaks to build with MinGW instead of MSVC:
# - Go only takes -fms-extensions from a dependency when allowed;
# - one MSVC-only __try/__except block, which just names a thread for the
#   Visual Studio debugger, is compiled out;
# - the Windows libraries MSVC would link on its own are listed by hand;
# - -static bundles the MinGW runtime, so the .exe needs no extra DLLs.
WIN_ENV := CGO_ENABLED=1 GOOS=windows GOARCH=amd64 \
	CC=$(WIN_CC) CXX=$(WIN_CXX) \
	CGO_CXXFLAGS_ALLOW='-fms-extensions' \
	CGO_CXXFLAGS='-O2 -D__try=if(0) -D__except(x)=else' \
	CGO_LDFLAGS='-ldbghelp -lbcrypt -lwinmm'

LDFLAGS := -s -w

.PHONY: all build windows test vet clean help check-linux check-windows

all: build windows ## Build for Linux and Windows

build: check-linux ## Build client and signaling server for this machine
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/ares ./cmd/ares
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/signal ./cmd/signal

windows: check-windows ## Cross-build bin/ares.exe and bin/signal.exe
	$(WIN_ENV) go build -ldflags '$(LDFLAGS) -extldflags "-static"' -o $(BIN)/ares.exe ./cmd/ares
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o $(BIN)/signal.exe ./cmd/signal

test: vet ## Vet and run every test with the race detector
	go test -race -count=5 ./...

vet: ## Static analysis
	go vet ./...

clean: ## Remove built binaries
	rm -rf $(BIN)

help: ## List the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

# Fail early with what to install, instead of a compiler error.
check-linux:
	@command -v go >/dev/null || { echo "Go is missing: install Go 1.27"; exit 1; }
	@command -v g++ >/dev/null || { echo "g++ is missing: sudo apt install build-essential"; exit 1; }
	@test "$$(uname)" != Linux || test -f /usr/include/X11/Xlib.h || \
		{ echo "X11 headers are missing: sudo apt install libx11-dev"; exit 1; }

check-windows:
	@command -v $(WIN_CXX) >/dev/null || { echo "$(WIN_CXX) is missing: sudo apt install mingw-w64"; exit 1; }
