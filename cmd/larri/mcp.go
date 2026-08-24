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

	"go.sovrenix.com/larri/internal/buildinfo"
	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/mcpsrv"
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

	// The server outlives its tool calls, so it can hold a rig — which is
	// what lets larri_up serve a model rather than merely provision one.
	sess := &tools.Session{}
	deps := tools.Deps{
		Store:           st,
		Session:         sess,
		HFToken:         secret.New(os.Getenv("HF_TOKEN")),
		NewOrchestrator: func(kind string) (*daemon.Orchestrator, error) { return newOrchestrator(st, kind, events) },
	}
	reg := tools.NewRegistry()
	if err := tools.Register(reg, deps); err != nil {
		return err
	}

	srv := &mcpsrv.Server{Registry: reg, Name: "larri", Version: buildinfo.Version()}
	fmt.Fprintf(os.Stderr, "larri mcp %s: %d tools on stdio\n", buildinfo.Version(), len(reg.All()))

	err = srv.Serve(ctx, os.Stdin, os.Stdout)

	// The host has gone. Stop holding the tunnel, but do **not** destroy: the
	// instance is still billing and the operator may want it. Saying so is
	// the difference between a rig they can reattach to and one they discover
	// on an invoice.
	if snap := sess.Snapshot(); snap.Running || snap.RigID != "" {
		sess.Stop()
		fmt.Fprintf(os.Stderr,
			"larri mcp: exiting with a rig still billing — reattach with 'larri resume', "+
				"or destroy it with 'larri down'\n")
	}
	return err
}

// newOrchestrator builds one configured from the environment.
func newOrchestrator(st *state.Store, runtimeKind string, events chan<- daemon.Event) (*daemon.Orchestrator, error) {
	prov, err := openProvider("")
	if err != nil {
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
		Store: st, Provider: prov, Runtime: eng,
		LabelSealer: sealer,
		Resolver:    sizing.NewHFResolver(secret.New(os.Getenv("HF_TOKEN"))),
		Policy:      rank.DefaultPolicy(),
		Deadline:    30 * time.Minute,
		Events:      events,

		// An agent-driven rig needs this most: nothing about an MCP host
		// guarantees it will still be there in an hour.
		IdleTimeout: config.Default().Idle.Timeout,
	}, nil
}
