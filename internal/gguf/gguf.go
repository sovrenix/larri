// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package gguf reads the metadata header of a GGUF file.
//
// It exists because a GGUF model carries its own architecture — layer count,
// embedding width, KV head count, trained context — in a header at the front
// of the file. That is the difference between sizing a quantised model and
// guessing at it, and LARRI refuses to guess (§7.1).
//
// Only the header is read. The architecture keys appear before the tokenizer
// vocabulary, which is megabytes of strings nothing here needs, so a truncated
// read is the normal case rather than an error: parsing stops when the buffer
// runs out and reports what it found.
package gguf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Magic is the four bytes every GGUF file starts with.
const Magic = "GGUF"

// value types, per the GGUF specification.
const (
	tUint8 uint32 = iota
	tInt8
	tUint16
	tInt16
	tUint32
	tInt32
	tFloat32
	tBool
	tString
	tArray
	tUint64
	tInt64
	tFloat64
)

// ErrNotGGUF reports a buffer that does not begin with the GGUF magic.
var ErrNotGGUF = errors.New("gguf: not a gguf file")

// File is the metadata a GGUF header carries.
type File struct {
	Version     uint32
	TensorCount uint64
	KV          map[string]any
}

// Arch returns the model architecture, e.g. "qwen2" or "llama". Every
// architecture-specific key is namespaced under it.
func (f *File) Arch() string {
	s, _ := f.KV["general.architecture"].(string)
	return s
}

// Uint reads an integer key, accepting any of the widths GGUF allows for it.
// Writers are not consistent about which they use, and a reader that insisted
// on one would fail on half the files in the wild.
func (f *File) Uint(key string) (uint64, bool) {
	switch v := f.KV[key].(type) {
	case uint64:
		return v, true
	case int64:
		if v >= 0 {
			return uint64(v), true
		}
	case float64:
		if v >= 0 && v == math.Trunc(v) {
			return uint64(v), true
		}
	}
	return 0, false
}

// ArchUint reads a key namespaced under the model's architecture, so callers
// ask for "block_count" rather than "qwen2.block_count".
func (f *File) ArchUint(suffix string) (uint64, bool) {
	a := f.Arch()
	if a == "" {
		return 0, false
	}
	return f.Uint(a + "." + suffix)
}

// String reads a string key.
func (f *File) String(key string) (string, bool) {
	s, ok := f.KV[key].(string)
	return s, ok
}

// Parse reads as much of the header as the buffer holds.
//
// A short buffer is expected: callers fetch a prefix of a file that may be
// gigabytes. Everything parsed before the buffer ran out is returned, and it
// is the caller's job to check that the keys it needs are present — which is a
// better contract than an error that discards the fields that did arrive.
func Parse(b []byte) (*File, error) {
	if len(b) < 24 || string(b[:4]) != Magic {
		return nil, ErrNotGGUF
	}
	p := &parser{b: b, off: 4}
	f := &File{KV: map[string]any{}}
	f.Version = p.u32()
	f.TensorCount = p.u64()
	n := p.u64()
	if p.err != nil {
		return nil, p.err
	}
	// A corrupt count could otherwise drive an unbounded loop over a buffer
	// that will never satisfy it.
	if n > 1<<20 {
		return nil, fmt.Errorf("gguf: implausible metadata count %d", n)
	}
	for i := uint64(0); i < n; i++ {
		key := p.str()
		typ := p.u32()
		val := p.value(typ)
		if p.err != nil {
			// Truncation is the normal end of a prefix read.
			break
		}
		f.KV[key] = val
	}
	if f.Version == 0 {
		return nil, fmt.Errorf("gguf: unreadable header")
	}
	return f, nil
}

type parser struct {
	b   []byte
	off int
	err error
}

func (p *parser) need(n int) bool {
	if p.err != nil {
		return false
	}
	if p.off+n > len(p.b) {
		p.err = errors.New("gguf: truncated")
		return false
	}
	return true
}

func (p *parser) u32() uint32 {
	if !p.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(p.b[p.off:])
	p.off += 4
	return v
}

func (p *parser) u64() uint64 {
	if !p.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(p.b[p.off:])
	p.off += 8
	return v
}

func (p *parser) str() string {
	n := p.u64()
	if n > uint64(len(p.b)) {
		p.err = errors.New("gguf: truncated")
		return ""
	}
	if !p.need(int(n)) {
		return ""
	}
	s := string(p.b[p.off : p.off+int(n)])
	p.off += int(n)
	return s
}

// value reads one metadata value, widening every integer to uint64/int64 so
// callers need not care which width a writer chose.
func (p *parser) value(t uint32) any {
	switch t {
	case tUint8:
		if !p.need(1) {
			return nil
		}
		v := uint64(p.b[p.off])
		p.off++
		return v
	case tInt8:
		if !p.need(1) {
			return nil
		}
		v := int64(int8(p.b[p.off]))
		p.off++
		return v
	case tUint16:
		if !p.need(2) {
			return nil
		}
		v := uint64(binary.LittleEndian.Uint16(p.b[p.off:]))
		p.off += 2
		return v
	case tInt16:
		if !p.need(2) {
			return nil
		}
		v := int64(int16(binary.LittleEndian.Uint16(p.b[p.off:])))
		p.off += 2
		return v
	case tUint32:
		return uint64(p.u32())
	case tInt32:
		return int64(int32(p.u32()))
	case tFloat32:
		return float64(math.Float32frombits(p.u32()))
	case tBool:
		if !p.need(1) {
			return nil
		}
		v := p.b[p.off] != 0
		p.off++
		return v
	case tString:
		return p.str()
	case tUint64:
		return p.u64()
	case tInt64:
		return int64(p.u64())
	case tFloat64:
		return math.Float64frombits(p.u64())
	case tArray:
		return p.array()
	default:
		p.err = fmt.Errorf("gguf: unknown value type %d", t)
		return nil
	}
}

// array skips over an array's contents.
//
// Nothing LARRI needs is inside one — the arrays in a model header are the
// tokenizer vocabulary and merges — so this advances past them rather than
// materialising megabytes of strings to discard.
func (p *parser) array() any {
	et := p.u32()
	n := p.u64()
	if p.err != nil {
		return nil
	}
	if et == tString {
		for i := uint64(0); i < n; i++ {
			if p.str(); p.err != nil {
				return nil
			}
		}
		return fmt.Sprintf("[%d strings]", n)
	}
	w, ok := fixedWidth(et)
	if !ok {
		p.err = fmt.Errorf("gguf: unsupported array element type %d", et)
		return nil
	}
	if n > uint64(len(p.b)) {
		p.err = errors.New("gguf: truncated")
		return nil
	}
	if !p.need(int(n) * w) {
		return nil
	}
	p.off += int(n) * w
	return fmt.Sprintf("[%d values]", n)
}

func fixedWidth(t uint32) (int, bool) {
	switch t {
	case tUint8, tInt8, tBool:
		return 1, true
	case tUint16, tInt16:
		return 2, true
	case tUint32, tInt32, tFloat32:
		return 4, true
	case tUint64, tInt64, tFloat64:
		return 8, true
	}
	return 0, false
}

// FileTypeBits reports the average bits per weight for a GGML file type.
//
// The values are the effective sizes of the quantisation schemes, k-quants
// included, so they are not whole numbers: Q4_K_M averages about 4.83 bits
// once its scales and mins are counted. Using 4 would understate a 30B model
// by gigabytes.
func FileTypeBits(ft uint64) (float64, bool) {
	switch ft {
	case 0: // all F32
		return 32, true
	case 1: // all F16
		return 16, true
	case 2: // Q4_0
		return 4.5, true
	case 3: // Q4_1
		return 5.0, true
	case 7: // Q8_0
		return 8.5, true
	case 8: // Q5_0
		return 5.5, true
	case 9: // Q5_1
		return 6.0, true
	case 10: // Q2_K
		return 2.63, true
	case 11, 12, 13: // Q3_K_S / M / L
		return 3.44, true
	case 14: // Q4_K_S
		return 4.58, true
	case 15: // Q4_K_M
		return 4.83, true
	case 16: // Q5_K_S
		return 5.52, true
	case 17: // Q5_K_M
		return 5.67, true
	case 18: // Q6_K
		return 6.56, true
	case 30: // BF16
		return 16, true
	}
	return 0, false
}

// FileTypeName renders a GGML file type as the quantisation name operators use.
func FileTypeName(ft uint64) string {
	names := map[uint64]string{
		0: "f32", 1: "f16", 2: "Q4_0", 3: "Q4_1", 7: "Q8_0", 8: "Q5_0", 9: "Q5_1",
		10: "Q2_K", 11: "Q3_K_S", 12: "Q3_K_M", 13: "Q3_K_L", 14: "Q4_K_S",
		15: "Q4_K_M", 16: "Q5_K_S", 17: "Q5_K_M", 18: "Q6_K", 30: "bf16",
	}
	if n, ok := names[ft]; ok {
		return n
	}
	return fmt.Sprintf("ggml-type-%d", ft)
}
