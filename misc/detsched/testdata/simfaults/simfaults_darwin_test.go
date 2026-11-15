// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simfaults

import (
	"net"
	"os"
	"syscall"
	"testing"
)

const ipRecvTOS = 0x1b // IP_RECVTOS

// mremap grows the shared mapping m of f to size bytes. Darwin has no
// mremap, so it maps the file again, as programs there do.
func mremap(m []byte, f *os.File, size int) ([]byte, error) {
	if err := syscall.Munmap(m); err != nil {
		return nil, err
	}
	return syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
}

// recvmmsg does nothing: darwin has no recvmmsg.
func recvmmsg(*net.UDPConn) {}

// checkRoutes does nothing: darwin has no netlink.
func checkRoutes(*testing.T) {}
