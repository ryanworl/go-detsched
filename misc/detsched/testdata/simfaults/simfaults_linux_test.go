// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simfaults

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

const ipRecvTOS = syscall.IP_RECVTOS

// mremap grows the shared mapping m of f to size bytes, letting the
// kernel move it.
func mremap(m []byte, _ *os.File, size int) ([]byte, error) {
	addr, _, e := syscall.Syscall6(syscall.SYS_MREMAP, uintptr(unsafe.Pointer(&m[0])), uintptr(len(m)), uintptr(size), 1, 0, 0) // MREMAP_MAYMOVE
	if e != 0 {
		return nil, e
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), size), nil
}

// recvmmsg reads the datagrams waiting on c with one recvmmsg.
func recvmmsg(c *net.UDPConn) {
	type mmsghdr struct {
		hdr syscall.Msghdr
		n   uint32
		_   [4]byte
	}
	var hs [8]mmsghdr
	bufs := make([][16]byte, 8)
	iovs := make([]syscall.Iovec, 8)
	for i := range hs {
		iovs[i].Base = &bufs[i][0]
		iovs[i].SetLen(16)
		hs[i].hdr.Iov = &iovs[i]
		hs[i].hdr.Iovlen = 1
	}
	c.SetReadDeadline(time.Time{})
	raw, _ := c.SyscallConn()
	var n uintptr
	var errno syscall.Errno
	raw.Read(func(fd uintptr) bool {
		n, _, errno = syscall.Syscall6(syscall.SYS_RECVMMSG, fd, uintptr(unsafe.Pointer(&hs[0])), 8, 0, 0, 0)
		return errno != syscall.EAGAIN
	})
	logf("udp recvmmsg: %d datagrams, first %d bytes, errno %d", n, hs[0].n, errno)
}

// checkRoutes checks the routing table a node at 10.0.0.7 reads over
// netlink.
func checkRoutes(t *testing.T) {
	rib, err := syscall.NetlinkRIB(syscall.RTM_GETROUTE, syscall.AF_UNSPEC)
	if err != nil {
		t.Error(err)
		return
	}
	msgs, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		t.Error(err)
		return
	}
	var routes []string
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWROUTE {
			continue
		}
		rt := (*syscall.RtMsg)(unsafe.Pointer(&m.Data[0]))
		attrs, err := syscall.ParseNetlinkRouteAttr(&m)
		if err != nil {
			t.Error(err)
			return
		}
		var dst, src net.IP
		for _, at := range attrs {
			switch at.Attr.Type {
			case syscall.RTA_DST:
				dst = net.IP(at.Value)
			case syscall.RTA_PREFSRC:
				src = net.IP(at.Value)
			}
		}
		routes = append(routes, fmt.Sprintf("%v/%d src %v", dst, rt.Dst_len, src))
	}
	if len(routes) != 1 || routes[0] != "10.0.0.0/8 src 10.0.0.7" {
		t.Errorf("routes = %q; want 10.0.0.0/8 src 10.0.0.7", routes)
	}
}
