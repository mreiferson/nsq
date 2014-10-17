package nsqd

import (
	"encoding/hex"
	"sync/atomic"
)

type guid int64

type guidFactory struct {
	sequence int64
}

func (f *guidFactory) NewGUID() guid {
	return guid(atomic.AddInt64(&f.sequence, 1))
}

func (g guid) Hex() MessageID {
	var h MessageID
	var b [8]byte

	b[0] = byte(g >> 56)
	b[1] = byte(g >> 48)
	b[2] = byte(g >> 40)
	b[3] = byte(g >> 32)
	b[4] = byte(g >> 24)
	b[5] = byte(g >> 16)
	b[6] = byte(g >> 8)
	b[7] = byte(g)

	hex.Encode(h[:], b[:])
	return h
}
