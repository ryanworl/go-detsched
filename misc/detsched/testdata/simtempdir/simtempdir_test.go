// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simtempdir creates temporary files and directories on a node,
// with TMPDIR set as macOS sets it, to a per-user directory under
// /var/folders. Before, nodes had only /tmp, so os.CreateTemp and
// os.MkdirTemp failed with ENOENT there. TestSimPrograms runs it.
package simtempdir

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func TestTempDir(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	os.Setenv("TMPDIR", "/var/folders/xy/abcdefgh/T/")
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		n := w.AddNode("a", "10.0.0.1")
		<-n.Start(func() {
			f, err := os.CreateTemp("", "snapshot")
			if err != nil {
				t.Errorf("CreateTemp: %v", err)
				return
			}
			f.Close()
			d, err := os.MkdirTemp("", "work")
			if err != nil {
				t.Errorf("MkdirTemp: %v", err)
				return
			}
			if filepath.Dir(f.Name()) != filepath.Clean(os.TempDir()) || filepath.Dir(d) != filepath.Clean(os.TempDir()) {
				t.Errorf("temporary names %s and %s are not in %s", f.Name(), d, os.TempDir())
			}
		})
	})
}
