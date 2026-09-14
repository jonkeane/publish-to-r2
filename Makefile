SHELL := /bin/bash
export GOMODCACHE := $(CURDIR)/.cache/gomod
export GOCACHE := $(CURDIR)/.cache/go-build
export GOTOOLCHAIN := go1.26.8

.PHONY: build helper test sdk-test package site-test
build: helper
	mkdir -p dist
	cd uploader && go build -trimpath -o ../dist/r2import ./cmd/r2import

helper:
	mkdir -p R2Publisher.lrplugin/bin
	cd uploader && CGO_ENABLED=1 go build -trimpath -o ../R2Publisher.lrplugin/bin/r2publisher ./cmd/r2publisher

test: helper
	cd uploader && go test -race ./...
	@for file in R2Publisher.lrplugin/*.lua; do luac -p "$$file" || exit 1; done
	lua tests/plugin.lua
	python3 tests/schemas.py

sdk-test:
	python3 tests/sdk_api.py "$(LIGHTROOM_SDK)"

package: build
	./scripts/package.sh
