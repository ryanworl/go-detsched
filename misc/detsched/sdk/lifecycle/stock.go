// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.detsched

package lifecycle

func setupComplete() {}

func branches() int { return 0 }
