// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Cryptorand prints values from crypto/rand and from crypto code that
// draws its own randomness.
package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
	"uuid"
)

func main() {
	fmt.Println(rand.Text())
	k, err := ecdh.X25519().GenerateKey(nil)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%x\n", k.PublicKey().Bytes())
	fmt.Println(uuid.New())
}
