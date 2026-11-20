// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/cpu"
	"internal/goarch"
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
//     Go, everywhere. On Linux the HWCAP words of the auxiliary vector
//     are masked the same way (see detAuxv), for golang.org/x/sys/cpu and
//     others that read them with getAuxv or from /proc/self/auxv.
//   - The auxiliary vector's length. The program sees the same entries in
//     the same order on every kernel (see detAuxvTags).
//   - The number of CPUs. GOMAXPROCS is 1 (see procresize), and NumCPU
//     returns detNumCPU, so that code sized by it, such as a pool of
//     workers, does the same work on every machine.
//   - The memory size is not read, and the kernel version only on paths
//     that leave the deterministic subset anyway, such as copying real
//     files or starting a process.
//
// What remains is what code reads itself: the CPUID instruction on x86,
// the ID registers on arm64, sysctl on darwin, files such as
// /proc/cpuinfo, /proc/self/mountinfo, and the cgroup's, and the page
// size (os.Getpagesize). The runtime describes the CPU and the page size
// in a machine ID (see detMachineID), which
// a search's runs print and its replay lines carry, so that a replay
// that runs another schedule on another machine says how the machines
// differ.

// detNumCPU is what NumCPU returns in det mode.
const detNumCPU = 4

// detHWCapKeep is the bits of AT_HWCAP that det mode keeps (see
// detAuxv), and 0 where it keeps the auxiliary vector as it is. On arm64:
// FP, ASIMD, AES, PMULL, SHA1, SHA2, CRC32, and ATOMICS, which the
// standard library chooses code by and every machine since Neoverse N1
// and Apple M1 has. It hides the rest, such as CPUID, with which code
// would read the ID registers instead, ASIMDDP, which SIMD libraries
// choose code by, and EVTSTRM, which some virtual machines lack. On x86,
// where AT_HWCAP is CPUID leaf 1's EDX: FPU, TSC, CX8, CMOV, MMX, FXSR,
// SSE, and SSE2, which every x86-64 machine has. AT_HWCAP2 and later are
// hidden entirely.
const detHWCapKeep = (1<<0|1<<1|1<<3|1<<4|1<<5|1<<6|1<<7|1<<8)*goarch.IsArm64 +
	(1<<0|1<<4|1<<8|1<<15|1<<23|1<<24|1<<25|1<<26)*(goarch.IsAmd64+goarch.Is386)

// detHWCap holds AT_HWCAP and AT_HWCAP2 as the kernel gave them, before
// detAuxv masked them, for the machine ID.
var detHWCap struct {
	set           bool
	hwcap, hwcap2 uintptr
}

// detMachineBuf holds the machine ID once made, outside the heap.
var detMachineBuf detMachineBufT

type detMachineBufT struct {
	n   int
	buf [160]byte
}

// detMachineID returns this machine's ID, fields separated by dots:
//
//	pPAGE      the page size, in decimal
//	fFEATURES  internal/cpu's features as detected, bit i for
//	           cpu.DetFeatureName(i)
//	hHWCAP     Linux: AT_HWCAP as the kernel gave it
//	gHWCAP2    Linux: AT_HWCAP2 as the kernel gave it
//	mMIDR      linux/arm64: the main ID register, when the kernel lets
//	           code read the ID registers (HWCAP_CPUID)
//	zDCZID     arm64: the data cache zero ID register, which gives the
//	           block size DC ZVA clears
//	sSIG       x86: CPUID leaf 1's EAX, the family, model, and stepping
//	cFAMILY    darwin/arm64: sysctl hw.cpufamily
//
// all but p in hex. Two machines with the same ID take the same code
// paths in code that reads the CPU itself.
func detMachineID() string {
	m := &detMachineBuf
	if m.n == 0 {
		m.field('p', uint64(physPageSize))
		m.field('f', cpu.DetRaw)
		if detHWCap.set {
			m.field('h', uint64(detHWCap.hwcap))
			m.field('g', uint64(detHWCap.hwcap2))
		}
		if GOARCH == "arm64" {
			if GOOS == "linux" && detHWCap.hwcap&detHWCapCPUID != 0 {
				m.field('m', detReadMIDR())
			}
			m.field('z', detReadDCZID())
		}
		if GOARCH == "amd64" || GOARCH == "386" {
			m.field('s', uint64(processorVersionInfo))
		}
		if v, ok := detCPUFamily(); ok {
			m.field('c', v)
		}
	}
	return slicebytetostringtmp(&m.buf[0], m.n)
}

// detHWCapCPUID is linux/arm64's HWCAP_CPUID: the kernel emulates reads
// of the ID registers.
const detHWCapCPUID = 1 << 11

// field adds field c with value v to the machine ID. It does not
// allocate, as the report at exit may not.
func (m *detMachineBufT) field(c byte, v uint64) {
	if m.n > 0 {
		m.put('.')
	}
	m.put(c)
	base := uint64(16)
	if c == 'p' {
		base = 10
	}
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
	for ; i < len(d); i++ {
		m.put(d[i])
	}
}

func (m *detMachineBufT) put(c byte) {
	if m.n < len(m.buf) {
		m.buf[m.n] = c
		m.n++
	}
}

// A detMachine is a parsed machine ID: the value of each field, by its
// letter, and which fields it has.
type detMachine struct {
	v   [26]uint64
	has uint32
}

func (d *detMachine) get(c byte) (uint64, bool) {
	return d.v[c-'a'], d.has&(1<<(c-'a')) != 0
}

// detMachineParse parses a machine ID.
func detMachineParse(id string) (d detMachine, ok bool) {
	for id != "" {
		c := id[0]
		if c < 'a' || c > 'z' {
			return d, false
		}
		base := uint64(16)
		if c == 'p' {
			base = 10
		}
		id = id[1:]
		var v uint64
		n := 0
		for ; n < len(id) && id[n] != '.'; n++ {
			x := id[n]
			var digit uint64
			switch {
			case '0' <= x && x <= '9':
				digit = uint64(x - '0')
			case 'a' <= x && x <= 'f' && base == 16:
				digit = uint64(x-'a') + 10
			default:
				return d, false
			}
			v = v*base + digit
		}
		if n == 0 {
			return d, false
		}
		d.v[c-'a'], d.has = v, d.has|1<<(c-'a')
		id = id[n:]
		if id != "" {
			id = id[1:]
		}
	}
	return d, true
}

// detMachineReport prints, at exit, what about this machine can make its
// schedules differ from other machines': the kept CPU features it lacks,
// and for a search's runs (GODETSEARCH's envread) its machine ID.
func detMachineReport() {
	missing := uint64(detHWCapKeep) &^ uint64(detHWCap.hwcap)
	if !detHWCap.set {
		missing = 0
	}
	if cpu.DetMissing != 0 || missing != 0 {
		print("detsched: warning: this machine lacks CPU features that det mode assumes every machine has:")
		detPrintFeatures(cpu.DetMissing)
		if missing != 0 {
			print(" HWCAP ", hex(missing))
		}
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
// ran another schedule, and how to find where the two runs part.
func detMachineDiffer() {
	there := detExpectMachine
	here := detMachineID()
	if !goexperiment.DetSched || there == "" || there == here {
		return
	}
	t, ok := detMachineParse(there)
	if !ok {
		return
	}
	h, _ := detMachineParse(here)
	print("detsched: it ran on another machine than the search's run (machine=", there, ", here ", here, "):")
	tr, _ := t.get('f')
	hr, _ := h.get('f')
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
	for _, f := range [...]struct {
		c    byte
		name string
		bits bool
	}{
		{'h', "HWCAP", true},
		{'g', "HWCAP2", true},
		{'m', "MIDR", false},
		{'z', "DCZID", false},
		{'s', "CPUID signature", false},
		{'c', "hw.cpufamily", false},
		{'p', "page size", false},
	} {
		tv, tok := t.get(f.c)
		hv, hok := h.get(f.c)
		switch {
		case tv == hv && tok == hok:
		case f.bits && tok && hok:
			print(" ", f.name, " bits only there ", hex(tv&^hv), ", only here ", hex(hv&^tv), ";")
		case f.c == 'p':
			print(" page size ", tv, " there, ", hv, " here;")
		default:
			print(" ", f.name, " ")
			if tok {
				print(hex(tv))
			} else {
				print("unknown")
			}
			print(" there, ")
			if hok {
				print(hex(hv))
			} else {
				print("unknown")
			}
			print(" here;")
		}
	}
	print(" det mode hides these from the standard library, but code that reads the CPU or the page size itself, such as golang.org/x/sys/cpu on x86 and darwin or github.com/klauspost/cpuid on x86, can choose other code by them, and so count other ticks, as can code that reads files that describe the machine, such as /proc/cpuinfo or the cgroup's\n")
}

// detBisectHint tells the user of a replay that ran another schedule than
// its replay line's how to find where the two runs part.
func detBisectHint() {
	print("detsched: to find where this run and the search's part, run the replay line here and where the search ran with GODETSTOP='s*100000' before GODETSEED=, and compare the stop lines: the first that differs brackets the switch; narrow it with GODETSTOP='sN*1000' from the last line that agrees, then to the tick with GODETSTOP='kN*100', and print both runs' stacks there with GODETSTOP=kN:dump\n")
}
