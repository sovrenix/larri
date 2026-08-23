// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package gguf

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// builder writes a GGUF header the way a real writer would.
type builder struct{ b bytes.Buffer }

func (w *builder) u32(v uint32) { binary.Write(&w.b, binary.LittleEndian, v) }
func (w *builder) u64(v uint64) { binary.Write(&w.b, binary.LittleEndian, v) }
func (w *builder) str(s string) {
	w.u64(uint64(len(s)))
	w.b.WriteString(s)
}
func (w *builder) kvStr(k, v string) { w.str(k); w.u32(tString); w.str(v) }
func (w *builder) kvU32(k string, v uint32) {
	w.str(k)
	w.u32(tUint32)
	w.u32(v)
}
func (w *builder) kvStrArray(k string, n int) {
	w.str(k)
	w.u32(tArray)
	w.u32(tString)
	w.u64(uint64(n))
	for i := 0; i < n; i++ {
		w.str("tok")
	}
}

func header(t *testing.T, kvCount int, build func(*builder)) []byte {
	t.Helper()
	w := &builder{}
	w.b.WriteString(Magic)
	w.u32(3)
	w.u64(338)
	w.u64(uint64(kvCount))
	build(w)
	return w.b.Bytes()
}

// The shape of a real model header, verified against
// registry.ollama.ai/library/qwen2.5:1.5b.
func TestParseReadsArchitecture(t *testing.T) {
	b := header(t, 8, func(w *builder) {
		w.kvStr("general.architecture", "qwen2")
		w.kvStr("general.size_label", "1.5B")
		w.kvU32("qwen2.block_count", 28)
		w.kvU32("qwen2.context_length", 32768)
		w.kvU32("qwen2.embedding_length", 1536)
		w.kvU32("qwen2.attention.head_count", 12)
		w.kvU32("qwen2.attention.head_count_kv", 2)
		w.kvU32("general.file_type", 15)
	})
	f, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if f.Arch() != "qwen2" {
		t.Errorf("arch = %q", f.Arch())
	}
	for _, c := range []struct {
		key  string
		want uint64
	}{
		{"block_count", 28}, {"embedding_length", 1536},
		{"attention.head_count_kv", 2}, {"context_length", 32768},
	} {
		got, ok := f.ArchUint(c.key)
		if !ok || got != c.want {
			t.Errorf("%s = %d (%v), want %d", c.key, got, ok, c.want)
		}
	}
}

// The normal case, not an error case: the architecture keys sit at the front
// and the tokenizer vocabulary is megabytes behind them, so a caller fetching
// a prefix always runs out mid-header. Everything read before that must
// survive — discarding it would defeat the point of a ranged read.
func TestTruncatedHeaderKeepsWhatItRead(t *testing.T) {
	b := header(t, 4, func(w *builder) {
		w.kvStr("general.architecture", "llama")
		w.kvU32("llama.block_count", 32)
		w.kvStrArray("tokenizer.ggml.tokens", 5000)
		w.kvU32("llama.embedding_length", 4096)
	})
	cut := b[:200] // mid-vocabulary

	f, err := Parse(cut)
	if err != nil {
		t.Fatalf("a truncated prefix should still parse: %v", err)
	}
	if f.Arch() != "llama" {
		t.Errorf("arch = %q", f.Arch())
	}
	if n, ok := f.ArchUint("block_count"); !ok || n != 32 {
		t.Errorf("block_count = %d (%v)", n, ok)
	}
	// And what lay beyond the cut is simply absent, not invented.
	if _, ok := f.ArchUint("embedding_length"); ok {
		t.Error("reported a key that was past the end of the buffer")
	}
}

func TestNonGGUFIsRefused(t *testing.T) {
	if _, err := Parse([]byte("not a gguf file at all, padding padding pad")); err != ErrNotGGUF {
		t.Errorf("err = %v, want ErrNotGGUF", err)
	}
}

// A corrupt count must not drive an unbounded loop over a buffer that can
// never satisfy it.
func TestImplausibleMetadataCountIsRefused(t *testing.T) {
	w := &builder{}
	w.b.WriteString(Magic)
	w.u32(3)
	w.u64(1)
	w.u64(1 << 40)
	if _, err := Parse(w.b.Bytes()); err == nil {
		t.Error("accepted a metadata count of a trillion")
	}
}

// k-quants carry scales and mins alongside the weights, so their effective
// width is not the number in the name. Using 4 for Q4_K_M understates a 30B
// model by gigabytes — which is a VRAM plan that fails at load, after paying.
func TestKQuantBitsAreNotTheNameNumber(t *testing.T) {
	bits, ok := FileTypeBits(15) // Q4_K_M
	if !ok {
		t.Fatal("Q4_K_M unknown")
	}
	if bits <= 4.0 {
		t.Errorf("Q4_K_M = %g bits; the scales and mins are unaccounted for", bits)
	}
	if FileTypeName(15) != "Q4_K_M" {
		t.Errorf("name = %q", FileTypeName(15))
	}
	if _, ok := FileTypeBits(9999); ok {
		t.Error("claimed to know an unknown quantisation")
	}
}
