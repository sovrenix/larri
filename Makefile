# Copyright (C) 2026 Sovrenix Inc.
# SPDX-License-Identifier: GPL-3.0-or-later

GO      ?= go
PKGS    := ./...
PKG     := go.sovrenix.com/larri/internal/buildinfo

# The version lives in the source, not here. A Makefile that invented one would
# make `go install` and `make build` disagree about what a user is running, and
# only one of them would be reproducible.
#
# A tag overrides it, but only a tag that looks like a release. `git describe`
# falls back to a bare commit hash when no tag exists, and stamping that as the
# version would replace 0.9.0 with something that is not a version at all.
TAG     := $(shell git describe --tags --exact-match --match 'v[0-9]*' 2>/dev/null | sed 's/^v//')
COMMIT  := $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
DIRTY   := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo -dirty)
# SOURCE_DATE_EPOCH keeps a build reproducible when a packager sets it.
DATE    := $(shell date -u -d "@$${SOURCE_DATE_EPOCH:-$$(date +%s)}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w -X '$(PKG).commit=$(COMMIT)$(DIRTY)' -X '$(PKG).date=$(DATE)'
ifneq ($(TAG),)
LDFLAGS += -X '$(PKG).version=$(TAG)'
endif

.PHONY: all build test race vet fmt headers check version version-check clean stale-check refresh-image site-extract site-embed

all: check build

build:
	CGO_ENABLED=0 $(GO) build -ldflags '$(LDFLAGS)' -o bin/larri ./cmd/larri

test:
	$(GO) test $(PKGS)

race:
	$(GO) test -race $(PKGS)

vet:
	$(GO) vet $(PKGS)

# gofmt must print nothing.
fmt:
	@out=$$(gofmt -l . ); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@echo "gofmt ok"

headers:
	@./scripts/check-headers.sh

# refresh-image re-reads the runtime image's digest and the hardware facts
# derived from it. The floors are not free-standing numbers: they belong to
# one specific build, and a moving tag invalidates them silently.
.PHONY: refresh-image
refresh-image:
	@$(GO) run ./internal/devtools/refreshimage $(IMAGE)
	@echo
	@echo "if these differ from internal/runtime/vllm/vllm.go, update all three together"

# The site's index.html is a generated bundle: 200 KB of base64 assets and a
# loader, with the actual page held as a JSON string in one <script> block.
# These two targets take that string out as real HTML and put it back, which
# is the only sane way to change a word on the page.
.PHONY: site-extract site-embed
site-extract:
	@$(GO) run ./internal/devtools/sitecontent extract

site-embed:
	@$(GO) run ./internal/devtools/sitecontent embed

# stale-check guards the mistake that costs real money: running bin/larri
# after changing the code that it was built from. A live verification once
# rented two GPUs to test a fix the binary did not contain, and reported the
# old failure as though the fix had not worked.
.PHONY: stale-check
stale-check:
	@if [ -x bin/larri ]; then \
		newer=$$(find . -name '*.go' -newer bin/larri -not -path './bin/*' -print -quit); \
		if [ -n "$$newer" ]; then \
			echo "bin/larri is older than $$newer — run 'make build'"; exit 1; \
		fi; \
		echo "bin/larri is up to date"; \
	fi

check: fmt vet headers test race version-check stale-check

clean:
	rm -rf bin

# version prints what a build would report.
version:
	@$(GO) run ./cmd/larri version

# version-check fails when a release tag disagrees with the source.
#
# Without it, tagging v1.0.0 against a tree that still says 0.9.0 produces a
# binary that reports the wrong release — and the tag is the thing people trust.
# No tag is not a failure; it is the normal state between releases.
version-check:
	@src=$$(sed -n 's/^var version = "\(.*\)"$$/\1/p' internal/buildinfo/buildinfo.go); \
	tag=$$(git describe --tags --exact-match --match 'v[0-9]*' 2>/dev/null | sed 's/^v//'); \
	if [ -z "$$tag" ]; then echo "version ok: $$src (untagged)"; exit 0; fi; \
	if [ "$$src" != "$$tag" ]; then \
		echo "version mismatch: source says $$src, tag says $$tag"; exit 1; fi; \
	echo "version ok: $$src"
