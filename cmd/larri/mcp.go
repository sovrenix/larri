// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/mcpsrv"
	"go.sovrenix.com/larri/internal/provider/vastai"
	"go.sovrenix.com/larri/internal/rank"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sizing"
	"go.sovrenix.com/larri/internal/state"
	"go.sovrenix.com/larri/internal/tools"
)

// cmdMCP serves the tool surface to an external agent over stdio.
//
// Stdio and nothing else. An MCP server that listened on a socket would be a
// second path to the operations that spend money, reachable by anything on the
// machine and authenticated by nothing — and the agent hosts that drive this
// all launch a local process anyway.
func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	_ = fs.Parse(args)

	// FR-CFG-02, stated rather than assumed. Invocation.MCP exists for this
	// and was set nowhere: a stdio protocol server that ever opened a
	// terminal prompt would hand its host a process that produces no output
	// and never exits, which is the worst shape a hang can take.
	mode := config.DetectMode(config.Invocation{MCP: true}, os.Getenv)
	if mode.Interactive() {
		return errors.New("mcp must never run interactively")
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	// stdout carries the protocol, so nothing else may be written there. An
	// event line printed to stdout would corrupt the JSON-RPC stream and the
	// host would report LARRI as broken rather than noisy.
	events := make(chan daemon.Event, 64)
	go func() {
		for e := range events {
			fmt.Fprintf(os.Stderr, "  %-10s %s\n", e.Phase, e.Message)
		}
	}()
	defer close(events)

	deps := tools.Deps{
		Store:           st,
		NewOrchestrator: func(kind string) (*daemon.Orchestrator, error) { return newOrchestrator(st, kind, events) },
	}
	reg := tools.NewRegistry()
	if err := tools.Register(reg, deps); err != nil {
		return err
	}

	srv := &mcpsrv.Server{Registry: reg, Name: "larri", Version: version}
	fmt.Fprintf(os.Stderr, "larri mcp: %d tools on stdio\n", len(reg.All()))
	return srv.Serve(ctx, os.Stdin, os.Stdout)
}

// newOrchestrator builds one configured from the environment.
func newOrchestrator(st *state.Store, runtimeKind string, events chan<- daemon.Event) (*daemon.Orchestrator, error) {
	key := os.Getenv("VASTAI_API_KEY")
	if key == "" {
		return nil, errors.New("VASTAI_API_KEY is not set")
	}
	labelKey, _, err := config.ResolveLabelKey(os.Getenv, os.ReadFile)
	if err != nil {
		return nil, err
	}
	sealer, err := config.LabelSealer(labelKey)
	if err != nil {
		return nil, err
	}
	eng, err := pickRuntime(runtimeKind, core.ModelSpec{})
	if err != nil {
		return nil, err
	}
	return &daemon.Orchestrator{
		Store: st, Provider: vastai.New(secret.New(key)), Runtime: eng,
		LabelSealer: sealer,
		Resolver:    sizing.NewHFResolver(secret.New(os.Getenv("HF_TOKEN"))),
		Policy:      rank.DefaultPolicy(),
		Deadline:    30 * time.Minute,
		Events:      events,
	}, nil
}
