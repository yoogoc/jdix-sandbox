package apiserver

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// randomSuffix produces a short, unambiguous id fragment.
//
// Base32 without padding, lower-cased: the alphabet has no characters that look
// alike in a terminal, which matters because these ids get read aloud and typed
// by hand during incidents.
func randomSuffix() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}
