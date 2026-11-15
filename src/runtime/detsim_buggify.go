// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.detsched

package runtime

import _ "unsafe" // for go:linkname

// The functions of detsim.go that package detsched/sdk/buggify calls,
// pushed to it only in det builds, where it uses them; in stock builds it
// defines no-op versions itself, which these would collide with.

//go:linkname detsimBuggifyRand github.com/ryanworl/go-detsched/misc/detsched/sdk/buggify.runtime_rand
//go:linkname detsimBuggifyEpoch github.com/ryanworl/go-detsched/misc/detsched/sdk/buggify.runtime_epoch
