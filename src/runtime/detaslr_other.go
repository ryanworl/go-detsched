// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(darwin && !ios && arm64)

package runtime

// detNoASLR does nothing: only macOS needs it; see detaslr_darwin.go.
func detNoASLR() {}
