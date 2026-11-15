// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package sim

// System calls package syscall has no names for.
const (
	sysRenameat2     = 316
	sysCopyFileRange = 326
	sysStatx         = 332
	sysOpenat2       = 437
	sysFaccessat2    = 439
	sysNewfstatat    = 262
	sysSendmmsg      = 307
	sysMemfdCreate   = 319
)
