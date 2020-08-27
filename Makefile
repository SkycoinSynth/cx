.DEFAULT_GOAL := help
.PHONY: build-parser build build-full test test-full update-golden-files
.PHONY: install-gfx-deps install-gfx-deps-LINUX install-gfx-deps-MSYS install-gfx-deps-MINGW install-gfx-deps-MACOS install-deps install install-full

PWD := $(shell pwd)

#PKG_NAMES_LINUX := glade xvfb libxinerama-dev libxcursor-dev libxrandr-dev libgl1-mesa-dev libxi-dev gir1.2-gtk-3.0 libgtk2.0-dev libperl-dev libcairo2-dev libpango1.0-dev libgtk-3-dev gtk+3.0 libglib2.0-dev
PKG_NAMES_LINUX := glade xvfb libxinerama-dev libxcursor-dev libxrandr-dev libgl1-mesa-dev libxi-dev libperl-dev libcairo2-dev libpango1.0-dev libglib2.0-dev libopenal-dev
#PKG_NAMES_MACOS := gtk gtk-mac-integration gtk+3 glade
PKG_NAMES_WINDOWS := mingw-w64-x86_64-openal

UNAME_S := $(shell uname -s)

ifneq (,$(findstring Linux, $(UNAME_S)))
PLATFORM := LINUX
SUBSYSTEM := LINUX
PACKAGES := PGK_NAMES_LINUX
DISPLAY  := :99.0
endif

ifneq (,$(findstring Darwin, $(UNAME_S)))
PLATFORM := MACOS
SUBSYSTEM := MACOS
PACKAGES := PKG_NAMES_MACOS
endif

ifneq (,$(findstring MINGW, $(UNAME_S)))
PLATFORM := WINDOWS
SUBSYSTEM := MINGW
PACKAGES := PKG_NAMES_WINDOWS
endif

#ifneq (,$(findstring CYGWIN, $(UNAME_S)))
#PLATFORM := WINDOWS
#SUBSYSTEM := CYGWIN
#endif

ifneq (,$(findstring MSYS, $(UNAME_S)))
PLATFORM := WINDOWS
SUBSYSTEM := MSYS
PACKAGES := PKG_NAMES_WINDOWS
endif

ifeq ($(PLATFORM), WINDOWS)
GOPATH := $(subst \,/,${GOPATH})
HOME := $(subst \,/,${HOME})
CXPATH := $(subst, \,/, ${CXPATH})
endif

INSTALL_GFX_DEPS := install-gfx-deps-$(SUBSYSTEM)

GLOBAL_GOPATH := $(GOPATH)
LOCAL_GOPATH  := $(HOME)/go

ifdef GLOBAL_GOPATH
  GOPATH := $(GLOBAL_GOPATH)
else
  GOPATH := $(LOCAL_GOPATH)
endif

## Ensure $GOBIN is set.
GOLANGCI_LINT_VERSION ?= latest
GOBIN ?= $(PWD)/bin
GO_OPTS ?= GOBIN=$(GOBIN)

ifdef CXPATH
	CX_PATH := $(CXPATH)
else
	CX_PATH := $(HOME)/cx
endif

ifeq ($(UNAME_S), Linux)
endif

configure-workspace: ## Configure CX workspace environment
	mkdir -p $(CX_PATH)/src $(CX_PATH)/bin $(CX_PATH)/pkg
	@echo "NOTE:\tCX workspace at $(CX_PATH)"

build-parser: install-deps ## Generate lexer and parser for CX grammar
	$(GOBIN)/nex -e cxgo/cxgo0/cxgo0.nex
	$(GOBIN)/goyacc -o cxgo/cxgo0/cxgo0.go cxgo/cxgo0/cxgo0.y
	$(GOBIN)/nex -e cxgo/parser/cxgo.nex
	$(GOBIN)/goyacc -o cxgo/parser/cxgo.go cxgo/parser/cxgo.y

build: build-parser ## Build CX from sources
	$(GO_OPTS) go mod vendor
	$(GO_OPTS) go build -tags="base" -i -o $(GOBIN)/cx github.com/SkycoinProject/cx/cxgo/
	chmod +x $(GOBIN)/cx

build-full: install-full build-parser ## Build CX from sources with all build tags
	$(GO_OPTS) go mod vendor
	$(GO_OPTS) go build -tags="base cxfx" -i -o $(GOBIN)/cx github.com/SkycoinProject/cx/cxgo/
	chmod +x $(GOBIN)/cx

build-android: install-full install-mobile build-parser
	# TODO @evanlinjin: We should switch this to use 'github.com/SkycoinProject/gomobile' once it can build.
	$(GO_OPTS) go get -u golang.org/x/mobile/cmd/gomobile

install-gfx-deps-LINUX:
	@echo 'Installing dependencies for $(UNAME_S)'
	sudo apt-get update -qq
	sudo apt-get install -y $(PKG_NAMES_LINUX) --no-install-recommends

install-gfx-deps-MSYS:
	@echo 'Installing dependencies for $(UNAME_S)'
	pacman -Sy
	pacman -S $(PKG_NAMES_WINDOWS)
	if [ ! -a /mingw64/lib/libOpenAL32.a]; then ln -s /mingw64/lib/libopenal.a /mingw64/lib/libOpenAL32.a; fi
	if [ ! -a /mingw64/lib/libOpenAL32.dll.a]; then ln -s /mingw64/lib/libopenal.dll.a /mingw64/lib/libOpenAL32.dll.a; fi

install-gfx-deps-MINGW: install-gfx-deps-MSYS

install-gfx-deps-MACOS:
	@echo 'Installing dependencies for $(UNAME_S)'
#brew install $(PKG_NAMES_MACOS)

install-deps:
	@echo "Installing go package dependencies"
	$(GO_OPTS) go get -u github.com/SkycoinProject/nex
	$(GO_OPTS) go get -u modernc.org/goyacc

install-gfx-deps: $(INSTALL_GFX_DEPS)
	# TODO evanlinjin: These are all dependencies. Do we need this here?
#	$(GO_OPTS) go get github.com/SkycoinProject/gltext
#	$(GO_OPTS) go get github.com/go-gl/gl/v3.2-compatibility/gl
#	$(GO_OPTS) go get github.com/go-gl/glfw/v3.3/glfw
#	$(GO_OPTS) go get golang.org/x/mobile/exp/audio/al
#	$(GO_OPTS) go get github.com/mjibson/go-dsp/wav

install: install-deps build configure-workspace ## Install CX from sources. Build dependencies
	@echo 'NOTE:\tWe recommend you to test your CX installation by running "cx ./tests"'
	$(GOBIN)/cx -v

install-full: install-gfx-deps install-deps build-full configure-workspace

install-mobile:
	$(GO_OPTS) go get golang.org/x/mobile/gl # TODO @evanlinjin: This is a library. needed?

install-linters: ## Install linters
	curl -sSfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | sh -s -- -b $(GOBIN) $(GOLANGCI_LINT_VERSION)
	$(GO_OPTS) go get -u golang.org/x/tools/cmd/goimports

lint: ## Run linters. Use make install-linters first.
	$(GOBIN)/golangci-lint run -c .golangci.yml ./cx

test: build ## Run CX test suite.
	$(GO_OPTS) go test -race -tags base github.com/SkycoinProject/cx/cxgo/
	cx ./lib/args.cx ./tests/main.cx ++wdir=./tests ++disable-tests=gui,issue

test-full: build ## Run CX test suite with all build tags
	$(GO_OPTS) go test -race -tags="base cxfx" github.com/SkycoinProject/cx/cxgo/
	cx ./lib/args.cx ./tests/main.cx ++wdir=./tests ++disable-tests=gui,issue

update-golden-files: build ## Update golden files used in CX test suite
	ls -1 tests/ | grep '.cx$$' | while read -r NAME; do echo "Processing $$NAME"; cx -t -co tests/testdata/tokens/$${NAME}.txt tests/$$NAME || true ; done

check-golden-files: update-golden-files ## Ensure golden files are up to date
	if [ "$(shell git diff tests/testdata | wc -l | tr -d ' ')" != "0" ] ; then echo 'Changes detected. Golden files not up to date' ; exit 2 ; fi

check: check-golden-files test ## Perform self-tests

format: ## Formats the code. Must have goimports installed (use make install-linters).
	goimports -w -local github.com/SkycoinProject/cx ./cx
	goimports -w -local github.com/SkycoinProject/cx ./cxgo/actions
	goimports -w -local github.com/SkycoinProject/cx ./cxgo/api

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'
