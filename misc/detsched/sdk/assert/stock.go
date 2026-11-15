// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.detsched

package assert

import (
	"fmt"
	"os"
	"sync"
)

var printed sync.Map // messages of failures already printed

func emit(kind int, cond, failed bool, message, details string) {
	if !failed {
		return
	}
	if _, dup := printed.LoadOrStore(message, true); !dup {
		fmt.Fprintf(os.Stderr, "assertion failed: %s %s\n", message, details)
	}
}
