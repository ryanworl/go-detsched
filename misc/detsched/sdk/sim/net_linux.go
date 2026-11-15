// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package sim

import (
	"encoding/binary"
	"syscall"
)

// The Linux frontend of the simulated network: sockaddr_in.

// The TCP keepalive options, for setsockopt.
const (
	tcpKeepIdle  = syscall.TCP_KEEPIDLE
	tcpKeepIntvl = syscall.TCP_KEEPINTVL
	tcpKeepCnt   = syscall.TCP_KEEPCNT
)

func readSockaddr(p, n uintptr) (addr, uintptr) {
	if n < 16 || p == 0 {
		return addr{}, uintptr(syscall.EINVAL)
	}
	b := mem(p, 16)
	if binary.LittleEndian.Uint16(b) != syscall.AF_INET {
		return addr{}, uintptr(syscall.EAFNOSUPPORT)
	}
	var a addr
	a.port = binary.BigEndian.Uint16(b[2:])
	copy(a.ip[:], b[4:8])
	return a, 0
}

func writeSockaddr(p, lenp uintptr, a addr) {
	if p == 0 || lenp == 0 {
		return
	}
	n := getInt32(lenp)
	var b [16]byte
	binary.LittleEndian.PutUint16(b[:], syscall.AF_INET)
	binary.BigEndian.PutUint16(b[2:], a.port)
	copy(b[4:8], a.ip[:])
	copy(mem(p, uintptr(max(n, 0))), b[:])
	putInt32(lenp, 16)
}

// Socket options and control messages of UDP sockets.
const (
	soRcvbufForce = 0x21 // SO_RCVBUFFORCE
	soSndbufForce = 0x20 // SO_SNDBUFFORCE
	ipTOS         = syscall.IP_TOS
	ipRecvTOS     = syscall.IP_RECVTOS
	ipPktinfo     = syscall.IP_PKTINFO
	ipRecvPktinfo = syscall.IP_PKTINFO
	udpSegment    = 103 // UDP_SEGMENT
)
