// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simwatch

import "syscall"

func newWatcher() (int, error) { return syscall.InotifyInit1(syscall.IN_CLOEXEC) }
