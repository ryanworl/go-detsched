// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package sim

// System calls package syscall has no names for.
const (
	sysRenameat2     = 276
	sysCopyFileRange = 285
	sysStatx         = 291
	sysOpenat2       = 437
	sysFaccessat2    = 439
	sysNewfstatat    = 79
	sysSendmmsg      = 269
	sysMemfdCreate   = 279
)
