// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import "cmd/internal/src"

// detCallPos reports the position of the call to runtime.detPreempt or
// runtime.detSync in b, the block that a tick or synchronization point
// calls it from. regalloc puts the reloads of the values the call
// clobbered at the end of that block, and gives them that position
// rather than the next use's: in a function that inlines a call where
// the check falls, the next use is in the inlined body, and a reload
// on this rarely taken path would then be the body's first instruction,
// where a debugger sets a breakpoint on the inlined function.
func detCallPos(b *Block) (src.XPos, bool) {
	for _, v := range b.Values {
		if c, ok := v.Aux.(*AuxCall); ok && c.Fn != nil {
			switch c.Fn.Name {
			case "runtime.detPreempt", "runtime.detSync":
				return v.Pos, true
			}
		}
	}
	return src.NoXPos, false
}
