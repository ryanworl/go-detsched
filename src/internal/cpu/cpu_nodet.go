// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !386 && !amd64 && !arm64

package cpu

// detFeatures is empty: det mode pins features only on 386, amd64, and
// arm64 (see DetPin).
var detFeatures []detFeature
