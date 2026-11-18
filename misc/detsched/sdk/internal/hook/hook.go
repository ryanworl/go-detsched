// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package hook connects packages of the SDK that do not import each
// other.
package hook

// AssertFailed, if set, is called with the message of every failing
// assertion. sim sets it while a World exists, to put failures in the
// fault timeline.
var AssertFailed func(msg string)
