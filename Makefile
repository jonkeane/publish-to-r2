SHELL := /bin/bash
export GOMODCACHE := $(CURDIR)/.cache/gomod
export GOCACHE := $(CURDIR)/.cache/go-build
export GOTOOLCHAIN := go1.26.8

.PHONY: build test sdk-test package site-test
build:
	cd uploader && CGO_ENABLED=1 go build -trimpath -o ../R2Publisher.lrplugin/bin/r2publisher ./cmd/r2publisher

test:
	cd uploader && go test -race ./...
	@for file in R2Publisher.lrplugin/*.lua; do luac -p "$$file" || exit 1; done
	lua tests/plugin.lua
	python3 tests/schemas.py

sdk-test:
	python3 tests/sdk_api.py "$(LIGHTROOM_SDK)"

site-test:
	cd site && go test ./cmd/r2import
	python3 tests/hugo.py

package: build
	./scripts/package.sh
