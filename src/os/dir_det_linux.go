// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package os

// runtime_detReadDir returns, under GOEXPERIMENT=detsched outside every
// simulated node, the whole directory open as fd as linux_dirent64
// records sorted by name, which the runtime reads without counting ticks
// (see runtime.os_runtime_detReadDir); ok is false elsewhere.
func runtime_detReadDir(fd uintptr) (records []byte, errno uintptr, ok bool)
