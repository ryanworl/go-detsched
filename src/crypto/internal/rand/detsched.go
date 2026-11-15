// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rand

import (
	"internal/byteorder"
	"internal/chacha8rand"
	"internal/goexperiment"
	"sync"
	_ "unsafe" // for go:linkname
)

// Under GOEXPERIMENT=detsched, all cryptographic randomness, including
// crypto/rand, comes from a ChaCha8 stream seeded from GODETSEED, as if
// every program called testing/cryptotest.SetGlobalRandom at startup.
// The output is predictable, so this is only for testing.
//
// This is not supported with GOFIPS140=v1.0.0, whose module has no
// testing reader.

func init() {
	if goexperiment.DetSched {
		SetTestingReader(nil)
	}
}

// detSeed returns GODETSEED. It is pushed by the runtime via linkname.
//
//go:linkname detSeed
func detSeed() uint64

type detReader struct {
	mu    sync.Mutex
	state chacha8rand.State
}

func newDetReader() *detReader {
	var seed [32]byte
	byteorder.LEPutUint64(seed[:8], detSeed())
	copy(seed[8:], "crypto/rand") // distinct from the runtime's stream
	r := new(detReader)
	r.state.Init(seed)
	return r
}

func (r *detReader) Read(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(b)
	for len(b) > 0 {
		x, ok := r.state.Next()
		if !ok {
			r.state.Refill()
			continue
		}
		var buf [8]byte
		byteorder.LEPutUint64(buf[:], x)
		b = b[copy(b, buf[:]):]
	}
	return n, nil
}
