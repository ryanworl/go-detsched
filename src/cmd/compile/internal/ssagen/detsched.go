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
	"cmd/internal/objabi"
)

// detPreempt reports whether fn counts ticks and synchronization points
// under GOEXPERIMENT=detsched, so that it is preempted deterministically.
// The runtime and the packages it imports are not counted: they may run
// where preemption is not allowed, and they are deterministic, so leaving
// them out only delays a preemption. Neither are the packages that wrap
// system calls:
//
//   - A goroutine that writes to standard error holds the write lock of
//     os.Stderr's poll.FD, and preempted there, a simulated node that
//     crashes would keep it from every other node for good. Without
//     ticks, nothing preempts the write, which det mode buffers.
//   - Their loops retry system calls interrupted by signals (EINTR),
//     which a debugger causes, so ticks there would make a run under a
//     debugger take another schedule.
//   - The length of the environment, which syscall copies, does not move
//     preemptions.
func detPreempt(fn *ir.Func, nosplit bool) bool {
	if !buildcfg.Experiment.DetSched || nosplit || fn.Linksym().ABIWrapper() {
		return false
	}
	switch path := base.Ctxt.Pkgpath; path {
	case "os", "syscall", "internal/poll", "internal/syscall/unix":
		return false
	default:
		return !objabi.LookupPkgSpecial(path).Runtime
	}
}

// detTick emits a function-entry tick for GOEXPERIMENT=detsched:
//
//	if runtime.detTicks--; runtime.detTicks <= 0 {
//		runtime.detPreempt()
//	}
//
// ssa.insertLoopReschedChecks emits the same check on loop backedges.
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
// is an atomic operation.
func detSyncFunc(sym *types.Sym) bool {
	return sym != nil && sym.Pkg != nil && (sym.Pkg.Path == "sync/atomic" || sym.Pkg.Path == "internal/runtime/atomic")
}
