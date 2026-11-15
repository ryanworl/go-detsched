// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(linux && (amd64 || arm64)) && !(darwin && !ios && arm64)

package runtime

// Fork mode needs Linux on amd64 or arm64, or macOS on arm64; see
// detfork.go.

func detForkInit(string) {
	fatal("GODETSEARCH's fork needs linux/amd64, linux/arm64, or darwin/arm64")
}

func detForkServe(pp *p) {}
