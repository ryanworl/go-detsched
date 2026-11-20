// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/cpu"
	"internal/goexperiment"
)

// The machine.
//
// A binary must run a seed's schedule on every machine of its GOOS and
// GOARCH, so nothing the runtime or the standard library reads about the
// machine may change the work that Go code does, which the schedule
// counts in ticks:
//
//   - CPU features. Package internal/cpu keeps only the features det mode
//     assumes every machine has, and hides the rest (see cpu.DetPin), so
//     that the standard library takes the same code paths, assembly or
//     Go, everywhere. On linux/arm64 the HWCAP words of the auxiliary
//     vector are masked the same way (see detAuxv), for
//     golang.org/x/sys/cpu and others that read them with getAuxv.
//   - The number of CPUs. GOMAXPROCS is 1 (see procresize), and NumCPU
//     returns detNumCPU, so that code sized by it, such as a pool of
//     workers, does the same work on every machine.
//   - The memory size is not read, and the kernel version only on paths
//     that leave the deterministic subset anyway, such as copying real
//     files or starting a process.
//
// What remains is what code reads itself: the CPUID instruction on x86,
// sysctl on darwin, /proc/cpuinfo, and the page size (os.Getpagesize).
// The runtime describes those in a machine ID (see detMachineID), which
// a search's runs print and its replay lines carry, so that a replay
// that runs another schedule on another machine says how the machines
// differ.

// detNumCPU is what NumCPU returns in det mode.
const detNumCPU = 4

// detHWCapKeep is the bits of linux/arm64's AT_HWCAP that det mode keeps:
// FP, ASIMD, EVTSTRM, AES, PMULL, SHA1, SHA2, CRC32, ATOMICS, FPHP,
// ASIMDHP, ASIMDRDM, LRCPC, DCPOP, and ASIMDDP, which every machine since
// Neoverse N1 and Apple M1 has. It hides CPUID, with which code would
// read the ID registers instead, and AT_HWCAP2 entirely.
const detHWCapKeep = 1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7 | 1<<8 | 1<<9 | 1<<10 | 1<<12 | 1<<15 | 1<<16 | 1<<20

// detHWCap holds linux/arm64's AT_HWCAP and AT_HWCAP2 as the kernel gave
// them, before detAuxv masked them, for the machine ID.
var detHWCap struct {
	set           bool
	hwcap, hwcap2 uintptr
}

// detMachineBuf holds the machine ID once made, outside the heap.
var detMachineBuf detMachineBufT

type detMachineBufT struct {
	n   int
	buf [80]byte
}

// detMachineID returns this machine's ID:
//
//	pPAGE.fFEATURES[.hHWCAP.gHWCAP2]
//
// the page size in decimal, internal/cpu's features as detected, bit i
// for cpu.DetFeatureName(i), and on linux/arm64 the HWCAP words, in hex.
// Two machines with the same ID take the same code paths in code that
// reads the CPU itself.
func detMachineID() string {
	m := &detMachineBuf
	if m.n == 0 {
		m.put("p")
		m.putUint(uint64(physPageSize), 10)
		m.put(".f")
		m.putUint(cpu.DetRaw, 16)
		if detHWCap.set {
			m.put(".h")
			m.putUint(uint64(detHWCap.hwcap), 16)
			m.put(".g")
			m.putUint(uint64(detHWCap.hwcap2), 16)
		}
	}
	return slicebytetostringtmp(&m.buf[0], m.n)
}

// put and putUint add s, and v in base 10 or 16, to the machine ID. They
// do not allocate, as the report at exit may not.
func (m *detMachineBufT) put(s string) {
	for i := 0; i < len(s) && m.n < len(m.buf); i++ {
		m.buf[m.n] = s[i]
		m.n++
	}
}

func (m *detMachineBufT) putUint(v uint64, base uint64) {
	var d [20]byte
	i := len(d)
	for {
		i--
		d[i] = "0123456789abcdef"[v%base]
		v /= base
		if v == 0 {
			break
		}
	}
	for ; i < len(d) && m.n < len(m.buf); i++ {
		m.buf[m.n] = d[i]
		m.n++
	}
}

// detMachineParse parses a machine ID.
func detMachineParse(id string) (page, raw uint64, hwcap, hwcap2 uint64, ok bool) {
	field := func(prefix byte, base uint64) uint64 {
		if !ok || len(id) == 0 || id[0] != prefix {
			ok = false
			return 0
		}
		id = id[1:]
		v, n := uint64(0), 0
		for n < len(id) && id[n] != '.' {
			c, d := id[n], uint64(0)
			switch {
			case '0' <= c && c <= '9':
				d = uint64(c - '0')
			case 'a' <= c && c <= 'f' && base == 16:
				d = uint64(c-'a') + 10
			default:
				ok = false
				return 0
			}
			v = v*base + d
			n++
		}
		id = id[n:]
		if len(id) > 0 {
			id = id[1:]
		}
		return v
	}
	ok = true
	page = field('p', 10)
	raw = field('f', 16)
	if ok && id != "" {
		hwcap = field('h', 16)
		hwcap2 = field('g', 16)
	}
	return
}

// detMachineReport prints, at exit, what about this machine can make its
// schedules differ from other machines': the kept CPU features it lacks,
// and for a search's runs (GODETSEARCH's envread) its machine ID.
func detMachineReport() {
	if cpu.DetMissing != 0 {
		print("detsched: warning: this machine lacks CPU features that det mode assumes every machine has:")
		detPrintFeatures(cpu.DetMissing)
		print("; the standard library runs other code here, so its schedules differ from other machines'\n")
	}
	if detEnvRead.report {
		print("detsched: machine=", detMachineID(), "\n")
	}
}

// detPrintFeatures prints the names of the features in mask, each after
// a space.
func detPrintFeatures(mask uint64) {
	for i := 0; i < 64; i++ {
		if mask&(1<<i) != 0 {
			if name := cpu.DetFeatureName(i); name != "" {
				print(" ", name)
			}
		}
	}
}

// detExpectMachine is GODETSEARCH's machine: the machine ID of the run a
// replay line replays, or "".
var detExpectMachine string

// detMachineDiffer prints how this machine differs from the one whose
// ID is detExpectMachine, if it does, for the warning of a replay that
// ran another schedule.
func detMachineDiffer() {
	there := detExpectMachine
	here := detMachineID()
	if !goexperiment.DetSched || there == "" || there == here {
		return
	}
	tp, tr, th, th2, ok := detMachineParse(there)
	if !ok {
		return
	}
	hp, hr, hh, hh2, _ := detMachineParse(here)
	print("detsched: it ran on another machine than the search's run (machine=", there, ", here ", here, "):")
	if tr&^hr != 0 {
		print(" CPU features only there:")
		detPrintFeatures(tr &^ hr)
		print(";")
	}
	if hr&^tr != 0 {
		print(" CPU features only here:")
		detPrintFeatures(hr &^ tr)
		print(";")
	}
	if th != hh || th2 != hh2 {
		print(" HWCAP bits only there ", hex(th&^hh), " ", hex(th2&^hh2), ", only here ", hex(hh&^th), " ", hex(hh2&^th2), ";")
	}
	if tp != hp {
		print(" page size ", tp, " there, ", hp, " here;")
	}
	print(" det mode hides these from the standard library, but code that reads the CPU or the page size itself, such as golang.org/x/sys/cpu on x86 and darwin or github.com/klauspost/cpuid, can choose other code by them, and so count other ticks\n")
}
