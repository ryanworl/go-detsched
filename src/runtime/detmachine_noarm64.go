// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !arm64

package runtime

func detReadMIDR() uint64  { return 0 }
func detReadDCZID() uint64 { return 0 }
