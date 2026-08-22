// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"crypto/rand"
	"fmt"
	"io"
	"strings"
	"time"
)

// crockford is Crockford base32: no I, L, O or U, so an ID read aloud or
// copied out of a terminal cannot be mistyped into a different valid ID.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// IDLen is the encoded length of a rig ID.
const IDLen = 26

// NewID mints a ULID: 48 bits of millisecond timestamp followed by 80 bits of
// randomness, encoded in Crockford base32.
//
// The timestamp prefix makes IDs sort lexically by creation time, so a
// directory listing of rigs/ is chronological and the journal reads in order
// without parsing anything.
//
// This is minted **before the first provider call** and stamped onto the
// provider resource as a label (FR-STATE-04). That ordering is what makes a
// crash between intent and create recoverable: the resource carries an ID that
// local state already knows about.
func NewID(t time.Time) (string, error) { return newID(t, rand.Reader) }

func newID(t time.Time, entropy io.Reader) (string, error) {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := io.ReadFull(entropy, b[6:]); err != nil {
		return "", fmt.Errorf("state: read entropy: %w", err)
	}
	return encodeULID(b), nil
}

// encodeULID renders 128 bits as 26 base32 digits. 26 digits carry 130 bits,
// so the value is treated as left-padded with two zero bits.
func encodeULID(b [16]byte) string {
	var out [IDLen]byte
	for i := 0; i < IDLen; i++ {
		// Bit offset into the 130-bit padded space, then into the real 128.
		hi := i*5 - 2
		var v byte
		for k := 0; k < 5; k++ {
			pos := hi + k
			v <<= 1
			if pos >= 0 {
				if b[pos/8]&(1<<(7-uint(pos%8))) != 0 {
					v |= 1
				}
			}
		}
		out[i] = crockford[v]
	}
	return string(out[:])
}

// ValidID reports whether s is a well-formed rig ID. Used before touching the
// filesystem with it, so a malformed ID cannot become a path.
func ValidID(s string) bool {
	if len(s) != IDLen {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune(crockford, r) {
			return false
		}
	}
	return true
}

// IDTime recovers the creation time encoded in an ID.
func IDTime(id string) (time.Time, error) {
	if !ValidID(id) {
		return time.Time{}, fmt.Errorf("state: malformed rig id %q", id)
	}
	var ms uint64
	for i := 0; i < 10; i++ { // 10 digits × 5 bits = 50 bits, top 2 are padding
		ms = ms<<5 | uint64(strings.IndexByte(crockford, id[i]))
	}
	ms &= (1 << 48) - 1
	return time.UnixMilli(int64(ms)).UTC(), nil
}
