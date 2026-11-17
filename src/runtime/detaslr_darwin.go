// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin && !ios && arm64

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"unsafe"
)

// macOS loads every program at a random address (ASLR), and the
// addresses of a program's types, functions, and globals reach its
// behavior: a map or sync.Map keyed by a reflect.Type, as encoders keep,
// hashes the type's address. Linux loads a Go program at a fixed address.
// Under GOEXPERIMENT=detsched, a program loaded at a random address runs
// itself again in its place without ASLR (posix_spawn with
// POSIX_SPAWN_SETEXEC and _POSIX_SPAWN_DISABLE_ASLR), before it does
// anything else.
//
// The Objective-C runtime crashes a child of fork that first uses a class
// with an +initialize method, if the parent had several threads, as Go
// programs do, unless OBJC_DISABLE_INITIALIZE_FORK_SAFETY is set when the
// process starts. Go reaches Objective-C through Security.framework, when
// crypto/x509 verifies a certificate with the system's verifier. Det mode
// forks (see detfork.go) only while every other Go thread is parked, so no
// +initialize is in progress on them; the run without ASLR starts with the
// variable set, and keeps it, as libobjc reads it when it is first used. A child cannot use the verifier if the parent
// used it before forking, though: Security.framework's connection to
// trustd does not survive fork. The program does not see the variable
// unless it was set already (see detEnvHidden), so that it sees the same
// environment as when it is started without ASLR, by a debugger say.

//go:cgo_import_dynamic libc_posix_spawn posix_spawn "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_init posix_spawnattr_init "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_setflags posix_spawnattr_setflags "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc__dyld_get_image_vmaddr_slide _dyld_get_image_vmaddr_slide "/usr/lib/libSystem.B.dylib"

func detaslr_posix_spawn_trampoline()
func detaslr_posix_spawnattr_init_trampoline()
func detaslr_posix_spawnattr_setflags_trampoline()
func detaslr_dyld_get_image_vmaddr_slide_trampoline()
func detaslr_getpid_trampoline()

// The variables the second run starts with, with C's NUL. The marker
// tells it that it is the second run, and, with value 2, that the first
// run added detObjCForkSafety right after it; it removes the marker from
// its environment before anything reads it.
const (
	detNoASLRMarker     = "GODETNOASLR=1\x00"
	detNoASLRMarkerObjC = "GODETNOASLR=2\x00"
	detObjCForkSafety   = "OBJC_DISABLE_INITIALIZE_FORK_SAFETY=YES\x00"
)

// detNoASLR runs the program again without ASLR, and without Objective-C's
// fork safety check, if the kernel loaded it at a random address. If it
// cannot, it says so and carries on. Called by randinit,
// before mallocinit, on m0 with no other threads, so it must not
// allocate.
func detNoASLR() {
	envp := (*[1 << 20]*byte)(add(unsafe.Pointer(argv), uintptr(argc+1)*goarch.PtrSize))
	n := 0
	for envp[n] != nil {
		n++
	}
	for i := 0; i < n; i++ {
		switch gostringnocopy(envp[i]) {
		case detNoASLRMarker[:len(detNoASLRMarker)-1]:
			copy(envp[i:n], envp[i+1:n+1])
			return
		case detNoASLRMarkerObjC[:len(detNoASLRMarkerObjC)-1]:
			copy(envp[i:n], envp[i+1:n+1])
			if i+1 < n && gostringnocopy(envp[i]) == detObjCForkSafety[:len(detObjCForkSafety)-1] {
				detEnvHidden = envp[i]
			}
			return
		}
	}
	// The slide of image 0, the executable, which is where the external
	// linker put it, too.
	slide, _, _ := syscall_rawsyscalln(abi.FuncPCABI0(detaslr_dyld_get_image_vmaddr_slide_trampoline), 0)
	if slide == 0 {
		return
	}
	if issetugid() == 1 {
		// The kernel would not mark the second run as set-ID, which
		// keeps the privileges, so it would run outside secure mode
		// (see secure).
		detNoASLRFailed("the program is set-user-ID or set-group-ID", 0)
		return
	}
	if executablePath == "" {
		detNoASLRFailed("the executable's path is unknown", 0)
		return
	}
	const (
		_POSIX_SPAWN_SETEXEC      = 0x40
		_POSIX_SPAWN_DISABLE_ASLR = 0x100
	)
	size := uintptr(n+3) * goarch.PtrSize
	p, err := mmap(nil, size, _PROT_READ|_PROT_WRITE, _MAP_ANON|_MAP_PRIVATE, -1, 0)
	if err != 0 {
		detNoASLRFailed("mmap failed", uintptr(err))
		return
	}
	env := unsafe.Slice((**byte)(p), n+3)
	copy(env, envp[:n])
	env[n] = unsafe.StringData(detNoASLRMarker)
	if v, _ := getenvEarly("OBJC_DISABLE_INITIALIZE_FORK_SAFETY"); v == "" {
		env[n] = unsafe.StringData(detNoASLRMarkerObjC)
		env[n+1] = unsafe.StringData(detObjCForkSafety)
	}
	if detTraced() {
		// The debugger's breakpoints are in this image, which the new
		// run replaces.
		print("detsched: running again without ASLR, so a debugger loses its breakpoints; start the program without ASLR (dlv exec --disable-aslr)\n")
	}
	var attr uintptr
	if r, _, _ := syscall_rawsyscalln(abi.FuncPCABI0(detaslr_posix_spawnattr_init_trampoline), uintptr(unsafe.Pointer(&attr))); r != 0 {
		detNoASLRFailed("posix_spawnattr_init failed", r)
		return
	}
	if r, _, _ := syscall_rawsyscalln(abi.FuncPCABI0(detaslr_posix_spawnattr_setflags_trampoline), uintptr(unsafe.Pointer(&attr)), _POSIX_SPAWN_SETEXEC|_POSIX_SPAWN_DISABLE_ASLR); r != 0 {
		detNoASLRFailed("posix_spawnattr_setflags failed", r)
		return
	}
	// On success, posix_spawn does not return.
	r, _, _ := syscall_rawsyscalln(abi.FuncPCABI0(detaslr_posix_spawn_trampoline), 0, uintptr(unsafe.Pointer(unsafe.StringData(executablePath))), 0, uintptr(unsafe.Pointer(&attr)), uintptr(unsafe.Pointer(argv)), uintptr(p))
	detNoASLRFailed("posix_spawn failed", r)
}

// detTraced reports whether a debugger traces this process (P_TRACED in
// its kinfo_proc's p_flag).
func detTraced() bool {
	const (
		_CTL_KERN      = 1
		_KERN_PROC     = 14
		_KERN_PROC_PID = 1
		_P_TRACED      = 0x800
		pFlagOffset    = 32 // of p_flag in struct kinfo_proc
	)
	pid, _, _ := syscall_rawsyscalln(abi.FuncPCABI0(detaslr_getpid_trampoline))
	mib := [4]uint32{_CTL_KERN, _KERN_PROC, _KERN_PROC_PID, uint32(pid)}
	var info [648 / 8]uint64 // struct kinfo_proc
	n := unsafe.Sizeof(info)
	if sysctl(&mib[0], 4, (*byte)(unsafe.Pointer(&info)), &n, nil, 0) != 0 {
		return false
	}
	return *(*int32)(unsafe.Add(unsafe.Pointer(&info), pFlagOffset))&_P_TRACED != 0
}

// detNoASLRFailed warns that the program runs at a random address, because
// the step named by what failed with error number errno (if not 0).
func detNoASLRFailed(what string, errno uintptr) {
	print("detsched: cannot run again without ASLR: ", what)
	if errno != 0 {
		print(" (errno ", errno, ")")
	}
	print("; schedules may differ between runs\n")
}
