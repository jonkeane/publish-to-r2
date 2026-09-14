SHELL := /bin/bash
export GOMODCACHE := $(CURDIR)/.cache/gomod
export GOCACHE := $(CURDIR)/.cache/go-build
export GOTOOLCHAIN := go1.26.8

.PHONY: build test sdk-test package site-test
build:
	mkdir -p R2Publisher.lrplugin/bin dist
	cd uploader && CGO_ENABLED=1 go build -trimpath -o ../R2Publisher.lrplugin/bin/r2publisher ./cmd/r2publisher
	cd uploader && go build -trimpath -o ../dist/r2import ./cmd/r2import

test:
	cd uploader && go test -race ./...
	@for file in R2Publisher.lrplugin/*.lua; do luac -p "$$file" || exit 1; done
	lua tests/plugin.lua
	python3 tests/schemas.py

sdk-test:
	python3 tests/sdk_api.py "$(LIGHTROOM_SDK)"

package: build
	./scripts/package.sh
