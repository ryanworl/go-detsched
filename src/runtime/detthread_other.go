// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !linux && !(darwin && arm64 && !ios)

package runtime

// detThreadSleeping reports true: where a thread's state cannot be read,
// the watchdog hands off the P of every long system call (see the Linux
// version).
func detThreadSleeping(mp *m) bool { return true }
