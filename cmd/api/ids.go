package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"
)

// newUUIDv7 returns a new primary-key ID: a version 7 UUID (RFC 9562).
//
// Layout: the first 48 bits are the Unix time in milliseconds, then version
// and variant bits, then random bits. Because the time comes first, IDs sort
// roughly by creation time, so inserts land at the end of the primary-key
// index (like an ascending key) instead of at random spots in it.
//
// IDs created within the same millisecond are unique but not ordered
// among themselves. Nothing here relies on that ordering.
func newUUIDv7() string {
	var b [16]byte
	rand.Read(b[:]) // crypto/rand never fails on supported platforms (Go 1.24+)

	// Overwrite bytes 0-5 with the timestamp. PutUint64 writes 8 bytes big-endian,
	// so write into a scratch buffer and copy the low 6 bytes.
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixMilli()))
	copy(b[0:6], ts[2:8])

	b[6] = (b[6] & 0x0f) | 0x70 // version 7 in the high nibble
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx (RFC 9562)

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// isUUID reports whether s looks like a UUID (8-4-4-4-12 hex digits). Handlers
// check IDs from requests with it, so a typo gets a clear 400 instead of a
// Postgres "invalid input syntax for type uuid" error.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f', 'A' <= c && c <= 'F':
		default:
			return false
		}
	}
	return true
}
