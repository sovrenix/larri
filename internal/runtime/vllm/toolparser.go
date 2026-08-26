// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package vllm

import "strings"

// ToolParserFor names the parser vLLM needs to turn a model's tool-call
// syntax into OpenAI tool calls, or "" when the family is not recognised.
//
// It exists because the failure is invisible until an agent tries. vLLM
// serves happily without it and answers ordinary chat, then refuses the first
// request carrying tools:
//
//	400 "auto" tool choice requires --enable-auto-tool-choice and
//	--tool-call-parser to be set
//
// An operator meets that inside their client, several steps from the launch
// flags that caused it, on a rig they have already paid to bring up. Naming
// the parser at launch costs nothing and removes the whole class.
//
// The names are vLLM's own, taken from its registry rather than from
// documentation: there are 47 of them and they are not guessable — Qwen2.5
// speaks Hermes syntax, Llama 3 wants llama3_json, and Llama 4 wants
// something different again. Only families whose parser is unambiguous are
// mapped; a model LARRI does not recognise gets no flag rather than a wrong
// one, because a mismatched parser produces malformed tool calls, which is
// worse than an honest refusal.
func ToolParserFor(ref string) string {
	r := strings.ToLower(ref)
	switch {
	// Qwen3's coder and XML variants have parsers of their own; the rest of
	// the Qwen line emits Hermes-style calls.
	case strings.Contains(r, "qwen3") && strings.Contains(r, "coder"):
		return "qwen3_coder"
	case strings.Contains(r, "qwen"):
		return "hermes"
	case strings.Contains(r, "hermes"), strings.Contains(r, "nousresearch"):
		return "hermes"
	case strings.Contains(r, "llama-4"), strings.Contains(r, "llama4"):
		return "llama4_pythonic"
	case strings.Contains(r, "llama-3"), strings.Contains(r, "llama3"):
		return "llama3_json"
	case strings.Contains(r, "mistral"), strings.Contains(r, "mixtral"),
		strings.Contains(r, "magistral"), strings.Contains(r, "devstral"):
		return "mistral"
	case strings.Contains(r, "deepseek-v3"), strings.Contains(r, "deepseek_v3"):
		return "deepseek_v3"
	case strings.Contains(r, "granite"):
		return "granite"
	case strings.Contains(r, "internlm"):
		return "internlm"
	case strings.Contains(r, "jamba"):
		return "jamba"
	case strings.Contains(r, "kimi-k2"), strings.Contains(r, "kimi_k2"):
		return "kimi_k2"
	case strings.Contains(r, "phi-4-mini"), strings.Contains(r, "phi4-mini"):
		return "phi4_mini_json"
	}
	return ""
}
