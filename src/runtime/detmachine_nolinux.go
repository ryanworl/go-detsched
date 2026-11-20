// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !linux

package runtime

func detKernelRelease() string     { return "" }
func detNofileMax() (uint64, bool) { return 0, false }
