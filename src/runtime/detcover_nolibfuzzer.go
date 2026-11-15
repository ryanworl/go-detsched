// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !libfuzzer

package runtime

// The linker places these at the start and end of the libfuzzer 8-bit
// counters of packages compiled with -d=libfuzzer, which detfeed.go
// reads with GODETSEARCH's cover field. libfuzzer.go declares them in libfuzzer builds.
var __start___sancov_cntrs, __stop___sancov_cntrs byte
