// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix

package synctest_test

import (
	"internal/goexperiment"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// TestDetSignalInBubble checks that, under detsched, a bubble can call
// signal.Notify and signal.Stop, as a library may when it opens a file,
// and that a program still gets signals outside its bubbles afterwards.
func TestDetSignalInBubble(t *testing.T) {
	if !goexperiment.DetSched {
		t.Skip("signal.Notify in a bubble needs detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGHUP)
		go func() {
			for range c {
			}
		}()
		time.Sleep(time.Second)
		signal.Stop(c)
		close(c)
	})
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGUSR1)
	defer signal.Stop(c)
	syscall.Kill(syscall.Getpid(), syscall.SIGUSR1)
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatal("no SIGUSR1 outside the bubble")
	}
}
