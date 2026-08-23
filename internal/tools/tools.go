// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package tools is the canonical definition of LARRI's operations as tools.
//
// Two drivers consume it — an external agent over MCP, and (later) the model on
// the rig through the chat pane — and both adapt *this* registry rather than
// declaring their own. A `larri_down` that means something different depending
// on who called it is the failure this structure exists to prevent (§14.2).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Exposure says which drivers may see a tool.
type Exposure int

const (
	// ExposeBoth is the read-only set: safe for anything to call.
	ExposeBoth Exposure = iota
	// ExposeMCPOnly is the consequential set. It reaches an external agent,
	// which is driven by a person, but not the model on the rig unless the
	// operator opts in — a model that can rent hardware in response to its
	// own output is a spending loop with no human in it (§14.4.4).
	ExposeMCPOnly
)

// Tool is one operation.
type Tool struct {
	Name        string
	Description string
	Schema      Schema
	Exposure    Exposure

	// Consequential marks a tool that spends or destroys. Drivers surface it
	// so the deciding agent can say what it is about to do rather than
	// discover it afterwards (FR-UI-03).
	Consequential bool

	Handler func(ctx context.Context, args json.RawMessage) (any, error)
}

// Schema is a JSON Schema object describing a tool's arguments.
type Schema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties,omitempty"`
	Required   []string            `json:"required,omitempty"`
}

// Property is one argument.
type Property struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// Object builds an argument schema.
func Object(props map[string]Property, required ...string) Schema {
	if props == nil {
		props = map[string]Property{}
	}
	sort.Strings(required)
	return Schema{Type: "object", Properties: props, Required: required}
}

// Registry holds the tools a driver may serve.
type Registry struct {
	tools map[string]Tool
}

func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Add registers a tool, refusing a duplicate name.
//
// Refusing rather than overwriting: two definitions under one name is exactly
// the divergence this package exists to prevent, and silently keeping the last
// one registered would make which definition wins depend on file order.
func (r *Registry) Add(t Tool) error {
	if t.Name == "" {
		return fmt.Errorf("tools: unnamed tool")
	}
	if t.Handler == nil {
		return fmt.Errorf("tools: %s has no handler", t.Name)
	}
	if _, dup := r.tools[t.Name]; dup {
		return fmt.Errorf("tools: %s registered twice", t.Name)
	}
	r.tools[t.Name] = t
	return nil
}

// MustAdd panics on a duplicate. For package-level registration, where the
// error is a programming mistake rather than a runtime condition.
func (r *Registry) MustAdd(t Tool) {
	if err := r.Add(t); err != nil {
		panic(err)
	}
}

// For returns the tools a driver may see, in a stable order.
func (r *Registry) For(e Exposure) []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		if e == ExposeBoth && t.Exposure == ExposeMCPOnly {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// All returns every registered tool, in a stable order.
func (r *Registry) All() []Tool { return r.For(ExposeMCPOnly) }

// Lookup finds a tool by name.
func (r *Registry) Lookup(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Call runs a tool by name.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (any, error) {
	t, ok := r.tools[name]
	if !ok {
		return nil, fmt.Errorf("tools: no tool %q", name)
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return t.Handler(ctx, args)
}
