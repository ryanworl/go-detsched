// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssagen

import (
	"internal/buildcfg"
	"strings"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/src"
)

// detCounts reports whether code at pos counts ticks and synchronization
// points: whether the source function it belongs to does, which for
// inlined code is the inlined function (see ir.DetFuncCounts). Nothing
// counts in a function that cannot call the scheduler.
func (s *state) detCounts(pos src.XPos) bool {
	if !buildcfg.Experiment.DetSched || base.Flag.CompilingRuntime || s.f.NoSplit || s.curfn.ABIWrapper() {
		return false
	}
	if i := base.Ctxt.PosTable.Pos(pos).Base().InliningIndex(); i >= 0 {
		return ir.DetInlCounts[i]
	}
	return s.detFunc
}

// detInlCounts reports whether the inlined call with inline tree index i
// counts the tick of its callee's entry.
func (s *state) detInlCounts(i int64) bool {
	return buildcfg.Experiment.DetSched && !base.Flag.CompilingRuntime && !s.f.NoSplit && !s.curfn.ABIWrapper() && ir.DetInlCounts[i]
}

// detTick emits a function-entry tick for GOEXPERIMENT=detsched:
//
//	if runtime.detTicks--; runtime.detTicks <= 0 {
//		runtime.detPreempt()
//	}
//
// It is emitted at the entry of a function that counts and is not a leaf
// (ir.DetLeaf), at the start of each inlined body of one, at the end of each iteration of a for loop in
// code that counts, and before each backward goto in its source.
func (s *state) detTick() {
	s.detCheck(ir.Syms.DetTicks, ir.Syms.DetPreempt)
}

// detSync emits a synchronization point, where scheduling perturbation
// may make the goroutine yield before the operation that follows:
//
//	if runtime.detSyncs--; runtime.detSyncs <= 0 {
//		runtime.detSync()
//	}
//
// Concurrency bugs depend on the order of synchronizing operations, and
// ticks fall only at function entries and loop backedges, so without
// these points a switch between, say, an atomic load and the store that
// depends on it would be rare. detSyncs counts separately from detTicks,
// so the points never move a tick preemption, and when perturbation is
// off they never yield.
func (s *state) detSync() {
	s.detCheck(ir.Syms.DetSyncs, ir.Syms.DetSync)
}

// detCheck emits: if *counter--; *counter <= 0 { fn() }.
func (s *state) detCheck(counter, fn *obj.LSym) {
	i32 := types.Types[types.TINT32]
	addr := s.entryNewValue1A(ssa.OpAddr, types.NewPtr(i32), counter, s.sb)
	ticks := s.newValue2(ssa.OpSub32, i32, s.load(i32, addr), s.constInt32(i32, 1))
	s.store(i32, addr, ticks)
	cmp := s.newValue2(ssa.OpLeq32, types.Types[types.TBOOL], ticks, s.constInt32(i32, 0))
	b := s.endBlock()
	b.Kind = ssa.BlockIf
	b.SetControl(cmp)
	b.Likely = ssa.BranchUnlikely
	bCall := s.f.NewBlock(ssa.BlockPlain)
	bNext := s.f.NewBlock(ssa.BlockPlain)
	b.AddEdgeTo(bCall)
	b.AddEdgeTo(bNext)
	s.startBlock(bCall)
	s.rtcall(fn, true, nil)
	s.endBlock().AddEdgeTo(bNext)
	s.startBlock(bNext)
}

// detSyncCall reports whether call n, a go statement if isGo, is a
// synchronization point: a go statement, a channel operation or select
// (which walk has turned into runtime calls), or an atomic operation that
// is not intrinsified.
func detSyncCall(n *ir.CallExpr, isGo bool) bool {
	if isGo {
		return true
	}
	if n.Op() != ir.OCALLFUNC || n.Fun.Op() != ir.ONAME || n.Fun.(*ir.Name).Class != ir.PFUNC {
		return false
	}
	sym := n.Fun.Sym()
	if sym.Pkg == ir.Pkgs.Runtime {
		switch sym.Name {
		case "chansend1", "chanrecv1", "chanrecv2", "closechan", "selectgo", "selectnbsend", "selectnbrecv":
			return true
		}
		return false
	}
	// Methods such as atomic.Int32.Add are compiled Go code whose
	// operations are synchronization points themselves.
	return detSyncFunc(sym) && !strings.Contains(sym.Name, ".")
}

// detSyncFunc reports whether a call to sym, which may be intrinsified,
// is an atomic operation: an exported function of the atomic packages,
// not a helper such as sync/atomic's b32, which an optimized build
// inlines.
func detSyncFunc(sym *types.Sym) bool {
	return sym != nil && sym.Pkg != nil && (sym.Pkg.Path == "sync/atomic" || sym.Pkg.Path == "internal/runtime/atomic") && types.IsExported(sym.Name)
}
