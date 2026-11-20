// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goarch"
	"internal/runtime/syscall/linux"
	"internal/stringslite"
	"unsafe"
)

// Files and system calls that describe the machine.
//
// Packages read the machine's description in their init, before any
// test runs: go.uber.org/automaxprocs reads /proc/self/cgroup,
// /proc/self/mountinfo, and the cgroup's cpu.max; memory-limit packages
// read memory.max, /proc/meminfo, or sysinfo(2); others count the
// processors in /proc/cpuinfo or /proc/stat, or read the CPUs online.
// Parsing them counts ticks by their contents, as long as each machine's,
// so in det mode the program sees one description, the same on every
// machine: four CPUs (as runtime.NumCPU returns, see detNumCPU) with the
// features det mode keeps, 16 GiB of memory, and a container at the root
// of a cgroup v2 hierarchy without CPU or memory limits. Opening one of
// these files outside every simulated node (whose file system is the
// simulation's) gives a file that holds det mode's contents instead, a
// cgroup v1 file does not exist, and sysinfo and sched_getaffinity for
// the process report the same machine. Other files and the process's
// real limits and affinity stay as they are.

// System calls det mode answers, and memfd_create and pwrite64 to make
// the files; 0 on other architectures.
const (
	detSysOpenat           = 257*goarch.IsAmd64 + 56*goarch.IsArm64
	detSysSysinfo          = 99*goarch.IsAmd64 + 179*goarch.IsArm64
	detSysSchedGetaffinity = 204*goarch.IsAmd64 + 123*goarch.IsArm64
	detSysMemfdCreate      = 319*goarch.IsAmd64 + 279*goarch.IsArm64
	detSysPwrite           = 18*goarch.IsAmd64 + 68*goarch.IsArm64
)

// detMemKB is the memory det mode's machine has, in KiB.
const detMemKB = 16 << 20

// detMachineFiles is the contents det mode gives files that describe the
// machine, by path, with /proc/thread-self/ and /proc/PID/ of the
// process's own ID read as /proc/self/.
var detMachineFiles = [...]struct{ path, content string }{
	{"/proc/meminfo", detMeminfo},
	{"/proc/stat", detProcStat},
	{"/proc/loadavg", "0.00 0.00 0.00 1/100 1\n"},
	{"/proc/self/cgroup", "0::/\n"},
	{"/proc/self/mountinfo", detMountinfo},
	{"/proc/self/mounts", detMounts},
	{"/proc/mounts", detMounts},
	{"/sys/devices/system/cpu/online", "0-3\n"},
	{"/sys/devices/system/cpu/possible", "0-3\n"},
	{"/sys/devices/system/cpu/present", "0-3\n"},
	{"/sys/fs/cgroup/cgroup.controllers", "cpuset cpu io memory hugetlb pids rdma misc\n"},
	{"/sys/fs/cgroup/cpu.max", "max 100000\n"},
	{"/sys/fs/cgroup/cpu.weight", "100\n"},
	{"/sys/fs/cgroup/cpuset.cpus.effective", "0-3\n"},
	{"/sys/fs/cgroup/cpuset.mems.effective", "0\n"},
	{"/sys/fs/cgroup/memory.max", "max\n"},
	{"/sys/fs/cgroup/memory.high", "max\n"},
	{"/sys/fs/cgroup/memory.low", "0\n"},
	{"/sys/fs/cgroup/memory.min", "0\n"},
	{"/sys/fs/cgroup/memory.swap.max", "max\n"},
	{"/sys/fs/cgroup/pids.max", "max\n"},
}

const detMeminfo = `MemTotal:       16777216 kB
MemFree:        15728640 kB
MemAvailable:   15728640 kB
Buffers:               0 kB
Cached:           524288 kB
SwapCached:            0 kB
Active:           262144 kB
Inactive:         262144 kB
Unevictable:           0 kB
Mlocked:               0 kB
SwapTotal:             0 kB
SwapFree:              0 kB
Dirty:                 0 kB
Writeback:             0 kB
AnonPages:        262144 kB
Mapped:            65536 kB
Shmem:                 0 kB
Slab:              65536 kB
SReclaimable:      32768 kB
SUnreclaim:        32768 kB
KernelStack:        4096 kB
PageTables:         4096 kB
CommitLimit:     8388608 kB
Committed_AS:    1048576 kB
VmallocTotal:   34359738367 kB
VmallocUsed:           0 kB
VmallocChunk:          0 kB
HugePages_Total:       0
HugePages_Free:        0
HugePages_Rsvd:        0
HugePages_Surp:        0
Hugepagesize:       2048 kB
`

const detProcStat = `cpu  400 0 400 40000 0 0 0 0 0 0
cpu0 100 0 100 10000 0 0 0 0 0 0
cpu1 100 0 100 10000 0 0 0 0 0 0
cpu2 100 0 100 10000 0 0 0 0 0 0
cpu3 100 0 100 10000 0 0 0 0 0 0
intr 0
ctxt 0
btime 946684800
processes 1
procs_running 1
procs_blocked 0
softirq 0 0 0 0 0 0 0 0 0 0 0
`

const detMountinfo = `22 1 0:21 / / rw,relatime - overlay overlay rw
23 22 0:22 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
24 22 0:23 / /sys ro,nosuid,nodev,noexec,relatime - sysfs sysfs ro
25 24 0:24 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw,nsdelegate
26 22 0:25 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755
27 22 0:26 / /tmp rw,nosuid,nodev - tmpfs tmpfs rw
`

const detMounts = `overlay / overlay rw,relatime 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
sysfs /sys sysfs ro,nosuid,nodev,noexec,relatime 0 0
cgroup /sys/fs/cgroup cgroup2 ro,nosuid,nodev,noexec,relatime,nsdelegate 0 0
tmpfs /dev tmpfs rw,nosuid,size=65536k,mode=755 0 0
tmpfs /tmp tmpfs rw,nosuid,nodev 0 0
`

// detCgroupV1 is the directories of cgroup v1 hierarchies, whose files
// do not exist on det mode's machine, which has only cgroup v2.
var detCgroupV1 = [...]string{
	"blkio", "cpu", "cpu,cpuacct", "cpuacct", "cpuacct,cpu", "cpuset",
	"devices", "freezer", "hugetlb", "memory", "net_cls", "net_cls,net_prio",
	"net_prio", "perf_event", "pids", "rdma", "systemd", "unified",
}

// /proc/cpuinfo's entry for each CPU, around its number.
const (
	detCPUInfoAmd64 = `vendor_id	: GenuineIntel
cpu family	: 6
model		: 85
model name	: Intel(R) Xeon(R) CPU @ 2.20GHz
stepping	: 7
microcode	: 0x1
cpu MHz		: 2200.000
cache size	: 39424 KB
physical id	: 0
siblings	: 4
`
	detCPUInfoAmd64b = `
cpu cores	: 4
fpu		: yes
fpu_exception	: yes
cpuid level	: 13
wp		: yes
flags		: fpu vme de pse tsc msr pae mce cx8 apic sep mtrr pge mca cmov pat pse36 clflush mmx fxsr sse sse2 ht syscall nx rdtscp lm constant_tsc rep_good nopl xtopology nonstop_tsc cpuid pni pclmulqdq ssse3 fma cx16 sse4_1 sse4_2 x2apic movbe popcnt aes xsave avx f16c rdrand hypervisor lahf_lm abm bmi1 avx2 bmi2 erms
bogomips	: 4400.00
clflush size	: 64
cache_alignment	: 64
address sizes	: 46 bits physical, 48 bits virtual
power management:

`
	detCPUInfoArm64 = `
BogoMIPS	: 50.00
Features	: fp asimd aes pmull sha1 sha2 crc32 atomics
CPU implementer	: 0x41
CPU architecture: 8
CPU variant	: 0x3
CPU part	: 0xd0c
CPU revision	: 1

`
)

// detCPUInfo holds /proc/cpuinfo once made, outside the heap: it is made
// on the system stack, which may not allocate.
var detCPUInfo detCPUInfoBuf

type detCPUInfoBuf struct {
	n   int
	buf [4096]byte
}

func (c *detCPUInfoBuf) put(s string) {
	c.n += copy(c.buf[c.n:], s)
}

func (c *detCPUInfoBuf) putDigit(d int) {
	c.buf[c.n] = byte('0' + d)
	c.n++
}

func detCPUInfoContent() string {
	c := &detCPUInfo
	if c.n == 0 {
		for i := 0; i < detNumCPU; i++ { // fewer than 10
			c.put("processor\t: ")
			c.putDigit(i)
			if GOARCH == "arm64" {
				c.put(detCPUInfoArm64)
				continue
			}
			c.put("\n")
			c.put(detCPUInfoAmd64)
			c.put("core id\t\t: ")
			c.putDigit(i)
			c.put("\napicid\t\t: ")
			c.putDigit(i)
			c.put("\ninitial apicid\t: ")
			c.putDigit(i)
			c.put(detCPUInfoAmd64b)
		}
	}
	return unsafe.String(&c.buf[0], c.n)
}

// detMachineFile returns the contents det mode gives the file at path,
// or found false for a file it leaves as it is, or enoent for one that
// does not exist on det mode's machine. It does not allocate.
func detMachineFile(path string) (content string, enoent, found bool) {
	// self is what follows /proc/self/, /proc/thread-self/, or /proc/PID/
	// of the process's own ID, if path starts with one.
	self, isSelf := stringslite.CutPrefix(path, "/proc/self/")
	if !isSelf {
		self, isSelf = stringslite.CutPrefix(path, "/proc/thread-self/")
	}
	if rest, ok := stringslite.CutPrefix(path, "/proc/"); !isSelf && ok && len(rest) > 0 && '1' <= rest[0] && rest[0] <= '9' {
		pid, i := 0, 0
		for ; i < len(rest) && '0' <= rest[i] && rest[i] <= '9'; i++ {
			pid = pid*10 + int(rest[i]-'0')
		}
		if i < len(rest) && rest[i] == '/' && pid == getpid() {
			self, isSelf = rest[i+1:], true
		}
	}
	if isSelf && self == "auxv" {
		return unsafe.String((*byte)(unsafe.Pointer(&detAuxvBuf[0])), int(unsafe.Sizeof(detAuxvBuf))), false, true
	}
	if path == "/proc/cpuinfo" {
		return detCPUInfoContent(), false, true
	}
	for _, f := range detMachineFiles {
		if f.path == path {
			return f.content, false, true
		}
		if fself, ok := stringslite.CutPrefix(f.path, "/proc/self/"); ok && isSelf && fself == self {
			return f.content, false, true
		}
	}
	if rest, ok := stringslite.CutPrefix(path, "/sys/fs/cgroup/"); ok {
		for _, d := range detCgroupV1 {
			if len(rest) > len(d) && rest[:len(d)] == d && rest[len(d)] == '/' {
				return "", true, true
			}
		}
	}
	return "", false, false
}

// syscall_detMachineSyscall answers, for package syscall's Syscall,
// Syscall6, and RawSyscall, the system calls of a goroutine on no
// simulated node that describe the machine: openat of one of its files
// (see detMachineFile), and sysinfo and sched_getaffinity. It reports
// whether it answered the call. It must not grow the stack: the
// arguments may point into it.
//
//go:linkname syscall_detMachineSyscall syscall.detMachineSyscall
//go:nosplit
func syscall_detMachineSyscall(trap, a1, a2, a3 uintptr) (r1, errno uintptr, ok bool) {
	if detSysOpenat == 0 || trap != detSysOpenat && trap != detSysSysinfo && trap != detSysSchedGetaffinity {
		return
	}
	gp := getg()
	if gp != gp.m.curg || detG(gp).node != 0 {
		return
	}
	systemstack(func() {
		r1, errno, ok = detMachineSyscall(trap, a1, a2, a3)
	})
	return
}

func detMachineSyscall(trap, a1, a2, a3 uintptr) (r1, errno uintptr, ok bool) {
	const (
		_ENOENT    = 2
		_EINVAL    = 22
		_O_CLOEXEC = 0x80000
	)
	// Not a switch: the traps are all 0 on other architectures.
	if trap == detSysOpenat {
		path := gostringnocopy((*byte)(unsafe.Pointer(a2)))
		if a3&3 != 0 || len(path) > 255 { // opened for writing, or not one
			return 0, 0, false
		}
		// With each run of slashes one, as the kernel reads it.
		var buf [256]byte
		m := 0
		for i := 0; i < len(path); i++ {
			if path[i] != '/' || m == 0 || buf[m-1] != '/' {
				buf[m] = path[i]
				m++
			}
		}
		path = unsafe.String(&buf[0], m)
		content, enoent, found := detMachineFile(path)
		if !found {
			return 0, 0, false
		}
		if enoent {
			return ^uintptr(0), _ENOENT, true
		}
		name := [...]byte{'d', 'e', 't', 0}
		var flags uintptr
		if a3&_O_CLOEXEC != 0 {
			flags = 1 // MFD_CLOEXEC
		}
		fd, _, e := linux.Syscall6(detSysMemfdCreate, uintptr(unsafe.Pointer(&name[0])), flags, 0, 0, 0, 0)
		if e != 0 {
			return 0, 0, false
		}
		// At offset 0, which leaves the file's offset at its start.
		n := uintptr(len(content))
		if n > 0 {
			if w, _, e := linux.Syscall6(detSysPwrite, fd, uintptr(unsafe.Pointer(unsafe.StringData(content))), n, 0, 0, 0); e != 0 || w != n {
				closefd(int32(fd))
				return 0, 0, false
			}
		}
		return fd, 0, true
	}
	if trap == detSysSysinfo {
		// struct sysinfo of 64-bit Linux, in units of 1 KiB.
		p := (*[14]uint64)(unsafe.Pointer(a1))
		*p = [14]uint64{}
		p[0] = 3600               // uptime
		p[4] = detMemKB           // totalram
		p[5] = detMemKB / 16 * 15 // freeram
		p[10] = 100               // procs, then padding
		p[13] = 1024              // mem_unit, then padding
		return 0, 0, true
	}
	if trap == detSysSchedGetaffinity {
		if a1 != 0 && int(a1) != getpid() {
			return 0, 0, false
		}
		if a2 < 8 {
			return ^uintptr(0), _EINVAL, true
		}
		*(*uint64)(unsafe.Pointer(a3)) = 1<<detNumCPU - 1
		return 8, 0, true
	}
	return 0, 0, false
}
