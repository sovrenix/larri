# Copyright (C) 2026 Sovrenix Inc.
# SPDX-License-Identifier: GPL-3.0-or-later

GO      ?= go
PKGS    := ./...
LDFLAGS := -s -w

.PHONY: all build test race vet fmt headers check clean

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

check: fmt vet headers test race

clean:
	rm -rf bin
