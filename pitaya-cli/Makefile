GO ?= go
GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)
BINARY_EXT = $(if $(filter windows,$(GOOS)),.exe,)
OUTPUT ?= ./out/pitaya-cli-$(GOOS)-$(GOARCH)$(BINARY_EXT)

.PHONY: build build-linux

build:
	@mkdir -p "$(dir $(OUTPUT))"
	@GOOS="$(GOOS)" GOARCH="$(GOARCH)" $(GO) build -o "$(OUTPUT)" .

build-linux:
	@$(MAKE) build GOOS=linux GOARCH=amd64
