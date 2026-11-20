// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simbubblefin sets finalizers and cleanups in a synctest bubble,
// on simulated nodes, that use the bubble's channels. They must run in
// their bubble, on their node, and not at all once their node crashed.
// Before, a GC epoch ran them on the finalizer goroutine, outside every
// bubble, and closing the channel ended the program with "close of
// synctest channel from outside bubble". TestSimPrograms runs it.
package simbubblefin

import (
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

type stream struct{ buf [64]byte }

// open returns a stream whose finalizer and cleanup close done and
// cleaned, and records the process they ran in.
//
//go:noinline
func open(done, cleaned chan struct{}, pid *atomic.Int64) {
	s := new(stream)
	runtime.SetFinalizer(s, func(*stream) {
		pid.Store(int64(os.Getpid()))
		close(done)
	})
	runtime.AddCleanup(s, func(c chan struct{}) { close(c) }, cleaned)
}

// TestLiveNode drops a stream on a node, which waits for its finalizer
// and cleanup.
func TestLiveNode(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		n := w.AddNode("a", "10.0.0.1")
		<-n.Start(func() {
			done, cleaned := make(chan struct{}), make(chan struct{})
			var pid atomic.Int64
			open(done, cleaned, &pid)
			runtime.GC()
			<-done
			<-cleaned
			if got := pid.Load(); got != int64(os.Getpid()) {
				t.Errorf("the finalizer ran in process %d, not the node's, %d", got, os.Getpid())
			}
		})
	})
}

// TestCrashedNode drops a stream on a node that then crashes. Its
// finalizer and cleanup must not run, as a crashed process runs none.
func TestCrashedNode(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		n := w.AddNode("a", "10.0.0.1")
		done, cleaned := make(chan struct{}), make(chan struct{})
		var pid atomic.Int64
		<-n.Start(func() { open(done, cleaned, &pid) })
		n.Kill()
		runtime.GC()
		runtime.GC()
		time.Sleep(time.Second)
		select {
		case <-done:
			t.Errorf("the finalizer of a crashed node's stream ran")
		case <-cleaned:
			t.Errorf("the cleanup of a crashed node's stream ran")
		default:
		}
	})
}

// TestIdleBubble waits in the bubble for a finalizer while a goroutine
// outside the bubble collects garbage, so that the epoch may queue the
// finalizer when every goroutine of the bubble is blocked. The bubble
// must neither report a deadlock nor move its clock before the
// finalizer ran, and must move it after.
func TestIdleBubble(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	var stop atomic.Bool
	defer stop.Store(true)
	go func() {
		for i := 0; i < 20 && !stop.Load(); i++ {
			runtime.GC()
			runtime.Gosched()
		}
	}()
	synctest.Test(t, func(t *testing.T) {
		done, cleaned := make(chan struct{}), make(chan struct{})
		var pid atomic.Int64
		open(done, cleaned, &pid)
		start := time.Now()
		<-done
		<-cleaned
		if d := time.Since(start); d != 0 {
			t.Errorf("the clock moved %v while the bubble waited for a finalizer", d)
		}
		time.Sleep(time.Hour)
		if d := time.Since(start); d != time.Hour {
			t.Errorf("after a sleep of an hour, the clock moved %v", d)
		}
	})
}
