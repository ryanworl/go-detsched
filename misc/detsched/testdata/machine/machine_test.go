// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package machine runs compute-bound goroutines that alternate at
// preemption points through the code paths a program takes by what it
// reads about the machine: hashes, ciphers, and signatures that package
// internal/cpu picks assembly or Go for, math/big, map hashing, workers
// sized by runtime.NumCPU, and the process ID and host name, and it reads
// $GODEBUG, as net/http and golang.org/x/sys/cpu do, so that a setting
// there such as dettrace=2 that it saw would move where it is preempted. Its schedule must be the same on every machine of a
// GOOS and GOARCH, and with or without dettrace.
package machine

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha3"
	"crypto/sha512"
	"hash/crc32"
	"hash/crc64"
	"math"
	"math/big"
	"math/bits"
	_ "net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

//go:noinline
func leaf(i int) int { return i*7 + 1 }

// step and other call, so each counts a tick at its entry, at its own
// position.
//
//go:noinline
func step(i int) int { return leaf(i) }

//go:noinline
func other(i int) int { return leaf(i) ^ 3 }

var mainSum int

// TestMain reads $GODEBUG a byte at a time, as golang.org/x/sys/cpu does,
// and then counts until it is preempted, so that a setting there that
// the program saw would move where.
func TestMain(m *testing.M) {
	for _, c := range []byte(os.Getenv("GODEBUG")) {
		if c == ',' {
			mainSum++
		}
	}
	for i := range 300000 {
		mainSum += step(i) + other(i)
	}
	os.Exit(m.Run())
}

// work is one goroutine's share of each kind of computation, which
// leaves the goroutine at a preemption point many times over.
func work(i int, sink *uint64) {
	buf := make([]byte, 4096+i)
	for j := range buf {
		buf[j] = byte(i*31 + j)
	}
	var s uint64
	add := func(b []byte) {
		for _, c := range b[:8] {
			s = s<<8 | uint64(c)
		}
	}
	for r := range 8 {
		h := sha256.Sum256(buf)
		add(h[:])
		h5 := sha512.Sum512(buf)
		add(h5[:])
		h1 := sha1.Sum(buf)
		add(h1[:])
		hm := md5.Sum(buf)
		add(hm[:])
		h3 := sha3.Sum256(buf)
		add(h3[:])
		mac := hmac.New(sha256.New, buf[:32])
		mac.Write(buf)
		add(mac.Sum(nil))
		s += uint64(crc32.ChecksumIEEE(buf)) + uint64(crc32.Checksum(buf, crc32.MakeTable(crc32.Castagnoli)))
		s += crc64.Checksum(buf, crc64.MakeTable(crc64.ECMA))

		block, _ := aes.NewCipher(buf[:32])
		gcm, _ := cipher.NewGCM(block)
		sealed := gcm.Seal(nil, buf[:12], buf[:1024], nil)
		add(sealed)
		ctr := cipher.NewCTR(block, buf[:16])
		out := make([]byte, 1024)
		ctr.XORKeyStream(out, buf[:1024])
		add(out)

		x := new(big.Int).SetBytes(buf[:256])
		y := new(big.Int).SetBytes(buf[256:512])
		m := new(big.Int).SetBytes(buf[512:640])
		z := new(big.Int).Exp(x, y, m.SetBit(m, 0, 1))
		add(z.Append(nil, 16))

		f := float64(i*r) + 0.5
		s += math.Float64bits(math.FMA(f, f, f)+math.Floor(f*1.5)+math.Ceil(f/3)+math.Trunc(f)) + uint64(bits.OnesCount64(s))

		keys := make(map[string]int)
		for k := range 64 {
			keys[strconv.Itoa(k*i)+string(buf[k:k+8])] = k
		}
		for k, v := range keys {
			s = s*31 + uint64(len(k)+v)
			break
		}
		s += uint64(strings.Count(string(buf), string(buf[100:103])) + strings.Index(string(buf), string(buf[3000:3010])))
		runtime.Gosched()
	}
	*sink = s
}

// sign signs and verifies with the keys of each kind, which draw their
// randomness from crypto/rand.
func sign(sink *uint64) {
	msg := []byte("a message to sign")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	sig := ed25519.Sign(priv, msg)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	h := sha256.Sum256(msg)
	esig, _ := ecdsa.SignASN1(rand.Reader, k, h[:])
	if ecdsa.VerifyASN1(&k.PublicKey, h[:], esig) {
		*sink += uint64(len(sig) + len(esig))
	}
}

func TestMachine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host, _ := os.Hostname()
		id := strconv.Itoa(os.Getpid()) + "@" + host
		var wg sync.WaitGroup
		sinks := make([]uint64, runtime.NumCPU()+8)
		for i := range sinks {
			wg.Go(func() {
				time.Sleep(time.Duration(i) * time.Millisecond)
				if i%4 == 0 {
					sign(&sinks[i])
				}
				work(i, &sinks[i])
				sinks[i] += uint64(len(id))
			})
		}
		wg.Wait()
	})
}
