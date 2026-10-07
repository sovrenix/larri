# Copyright (C) 2026 Sovrenix Inc.
# SPDX-License-Identifier: GPL-3.0-or-later
#
# LARRI as a container, for running the rig lifecycle from a cluster rather
# than a workstation.
#
# Two notes for anyone deploying this on OpenShift:
#
# The served endpoint binds 127.0.0.1 by design (internal/daemon/up.go and
# internal/wire/proxy.go). Inside a pod that is the pod's loopback, which is
# shared between containers in the same pod but reachable from nowhere else.
# Exposing it to the cluster is a sidecar's job, not a change to that bind --
# the loopback-only listener is a security property, not an oversight.
#
# The image runs under an arbitrary UID with GID 0, so HOME and the state
# directory are group-owned by 0 and group-writable.

FROM golang:1.25-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags "-s -w" -o /out/larri ./cmd/larri

FROM alpine:3.20
RUN apk add --no-cache ca-certificates openssh-client

COPY --from=build /out/larri /usr/local/bin/larri

ENV HOME=/var/lib/larri \
    XDG_CONFIG_HOME=/var/lib/larri/.config \
    XDG_STATE_HOME=/var/lib/larri/.state
RUN mkdir -p /var/lib/larri/.config /var/lib/larri/.state \
 && chgrp -R 0 /var/lib/larri \
 && chmod -R g=u /var/lib/larri

USER 1001
WORKDIR /var/lib/larri
ENTRYPOINT ["/usr/local/bin/larri"]
