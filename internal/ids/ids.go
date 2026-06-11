package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

func Random(prefix string, n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return Stable(prefix, time.Now().String())
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

func Stable(prefix string, parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return prefix + "_" + hex.EncodeToString(h.Sum(nil))[:24]
}
