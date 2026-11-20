// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// detReadMIDR returns MIDR_EL1, for the machine ID. It traps where the
// kernel does not emulate reads of the ID registers.
func detReadMIDR() uint64

// detReadDCZID returns DCZID_EL0, for the machine ID.
func detReadDCZID() uint64
