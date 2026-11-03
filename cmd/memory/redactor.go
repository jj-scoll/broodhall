package main

import (
	"crypto/sha256"
	"encoding/hex"
)

// redactor masks secrets in text. Masked values become [class:tag], where tag is a keyed
// hash of the value, so equal values keep equal tags.
type redactor struct {
	key []byte
}

func newRedactor(key []byte, _ []string) *redactor { return &redactor{key: key} }

// tag is [category:6 hex of sha256(hex(key) US category US value)], US = \x1f.
func (x *redactor) tag(category, value string) string {
	sum := sha256.Sum256([]byte(hex.EncodeToString(x.key) + "\x1f" + category + "\x1f" + value))
	return "[" + category + ":" + hex.EncodeToString(sum[:3]) + "]"
}
