// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package shuffle runs its tests with -test.shuffle=on, which picks
// a random order.
package shuffle

import (
	"flag"
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	flag.Parse()
	flag.Set("test.shuffle", "on")
	os.Exit(m.Run())
}

func TestA(t *testing.T) { fmt.Println("A") }
func TestB(t *testing.T) { fmt.Println("B") }
func TestC(t *testing.T) { fmt.Println("C") }
func TestD(t *testing.T) { fmt.Println("D") }
func TestE(t *testing.T) { fmt.Println("E") }
