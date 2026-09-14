// Package idgen produces ULID-like, lexicographically sortable identifiers.
//
// The ID is a 48-bit millisecond timestamp encoded in Crockford base32
// followed by a random part that is bumped when several IDs are generated
// inside the same millisecond, so IDs stay monotonic within a process and
// collision safe across processes.
package idgen

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"sync"
	"time"
)

// NewID returns a prefixed identifier such as "evt_01J8Q7ZC4K9W2M3N4P5Q6R7S".
func NewID(prefix string) string {
	return prefix + "_" + encodeTime(time.Now().UTC()) + randomString(12)
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	idMu       sync.Mutex
	lastMillis int64
	lastRandom [9]byte
)

func encodeTime(t time.Time) string {
	idMu.Lock()
	defer idMu.Unlock()

	millis := t.UnixMilli()
	if millis == lastMillis {
		// Same millisecond: bump the random part to keep IDs monotonic.
		incr(&lastRandom)
	} else {
		lastMillis = millis
		if _, err := rand.Read(lastRandom[:]); err != nil {
			for i := range lastRandom {
				lastRandom[i] = byte(time.Now().UnixNano() >> (8 * i))
			}
		}
	}
	return encodeUint(uint64(millis), 10)
}

func incr(b *[9]byte) {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return
		}
	}
}

func encodeUint(v uint64, length int) string {
	out := make([]byte, length)
	for i := length - 1; i >= 0; i-- {
		out[i] = crockford[v&31]
		v >>= 5
	}
	return string(out)
}

func randomString(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte(time.Now().UnixNano() >> (i % 8))
		}
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
	enc = strings.ToUpper(enc)
	if len(enc) >= n {
		return enc[:n]
	}
	return enc + strings.Repeat("0", n-len(enc))
}
