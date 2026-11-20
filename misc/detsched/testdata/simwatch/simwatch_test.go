// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simwatch creates a watcher of file system changes on a node, as
// a configuration reloader does: inotify on Linux, kqueue on macOS.
// Watchers are not simulated, so the call must fail with ENOSYS, and the
// simulation must say why. Before, it created a real watcher, whose
// goroutine waited on the OS, and adding a simulated path to it failed.
// TestSimPrograms runs it.
package simwatch

import (
	"errors"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func TestWatch(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		n := w.AddNode("a", "10.0.0.1")
		<-n.Start(func() {
			fd, err := newWatcher()
			if fd != -1 || !errors.Is(err, syscall.ENOSYS) {
				t.Errorf("creating a watcher on a node: descriptor %d, error %v; want ENOSYS", fd, err)
			}
		})
	})
}
