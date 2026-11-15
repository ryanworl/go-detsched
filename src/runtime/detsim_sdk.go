// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.detsched && ((linux && (amd64 || arm64)) || (darwin && !ios && arm64))

package runtime

import _ "unsafe" // for go:linkname

// The functions of detsim.go that package detsched/sdk/sim calls. They
// are pushed to it only in builds where it uses them (its det.go has the
// same build constraint); elsewhere it defines no-op versions itself, in
// stock.go, which these would collide with.

//go:linkname detsimRegister github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_register
//go:linkname detsimSetForked github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_onFork
//go:linkname detsimNodeOf github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_node
//go:linkname detsimSetNode github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_setNode
//go:linkname detsimCall github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_call
//go:linkname detsimCrash github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_crash
//go:linkname detsimHalt github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_halt
//go:linkname detsimPause github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_pause
//go:linkname detsimResume github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_resume
//go:linkname detsimCount github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_count
//go:linkname detsimStacks github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_stacks
//go:linkname detsimAfter github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_after
//go:linkname detsimNow github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_now
//go:linkname detsimReady github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_ready
//go:linkname detsimRand github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_rand
//go:linkname detsimEpoch github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_epoch
//go:linkname detsimSetClock github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_clock
//go:linkname detsimSetPanic github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_onPanic
//go:linkname detsimBranch github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_branch
//go:linkname detsimBusyOn github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_busyOn
//go:linkname detsimSetCPU github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_setCPU
//go:linkname detsimBusyAdd github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_busyAdd
//go:linkname detsimBusyStats github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_busyStats
//go:linkname detsimWait github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_wait
//go:linkname detsimWake github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_wake
//go:linkname detsimSetFailLog github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_failLog
//go:linkname detsimWatch github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.runtime_watch
