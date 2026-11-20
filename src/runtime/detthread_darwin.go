// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin && arm64 && !ios

package runtime

import (
	"internal/abi"
	"unsafe"
)

//go:cgo_import_dynamic libc_pthread_mach_thread_np pthread_mach_thread_np "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_thread_info thread_info "/usr/lib/libSystem.B.dylib"

func detthread_pthread_mach_thread_np_trampoline()
func detthread_thread_info_trampoline()

// detThreadSleeping reports whether mp's thread waits in the kernel for
// an event, TH_STATE_WAITING in its thread_basic_info, rather than
// running, waiting for a CPU, or waiting uninterruptibly, as for a disk.
// See the Linux version.
func detThreadSleeping(mp *m) bool {
	const (
		_THREAD_BASIC_INFO = 3
		_TH_STATE_WAITING  = 3
		runStateOffset     = 24 // of run_state in struct thread_basic_info
	)
	thread := uintptr(mp.procid)
	port := libcCall(unsafe.Pointer(abi.FuncPCABI0(detthread_pthread_mach_thread_np_trampoline)), unsafe.Pointer(&thread))
	if port == 0 {
		return true
	}
	var info [10]int32 // struct thread_basic_info
	count := uint32(len(info))
	args := struct {
		port   uintptr
		flavor uintptr
		info   *int32
		count  *uint32
	}{uintptr(uint32(port)), _THREAD_BASIC_INFO, &info[0], &count}
	if libcCall(unsafe.Pointer(abi.FuncPCABI0(detthread_thread_info_trampoline)), unsafe.Pointer(&args)) != 0 {
		return true
	}
	return info[runStateOffset/4] == _TH_STATE_WAITING
}
