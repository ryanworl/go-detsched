// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import (
	"cmd/compile/internal/types"
	"cmd/internal/src"
)

// detTickCheck makes test, a loop backedge check built by
// insertLoopReschedChecks, count a tick for GOEXPERIMENT=detsched instead
// of comparing the stack pointer with the stack limit, as ssagen's detTick
// does at function entry:
//
//	if runtime.detTicks--; runtime.detTicks <= 0 { goto sched }
//
// It returns the memory state after the tick is stored. The stack limit
// comparison it replaces is left for dead code elimination.
func detTickCheck(test *Block, mem *Value) *Value {
	f := test.Func
	pos := test.Pos
	i32 := f.Config.Types.Int32
	sb := f.Entry.NewValue0(f.Entry.Pos, OpSB, f.Config.Types.Uintptr)
	addr := f.Entry.NewValue1A(f.Entry.Pos, OpAddr, f.Config.Types.Int32Ptr, f.fe.Syslook("detTicks"), sb)
	ticks := test.NewValue2(pos, OpSub32, i32, test.NewValue2(pos, OpLoad, i32, addr, mem), test.NewValue0I(pos, OpConst32, i32, 1))
	mem = test.NewValue3A(pos, OpStore, types.TypeMem, i32, addr, ticks, mem)
	test.SetControl(test.NewValue2(pos, OpLeq32, f.Config.Types.Bool, ticks, test.NewValue0I(pos, OpConst32, i32, 0)))
	return mem
}

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
