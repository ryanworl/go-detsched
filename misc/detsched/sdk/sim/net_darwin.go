// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin && !ios && arm64

package sim

import (
	"encoding/binary"
	"syscall"
)

// The darwin frontend of the simulated network: sockaddr_in, which starts
// with its length.

// The TCP keepalive options, for setsockopt.
const (
	tcpKeepIdle  = syscall.TCP_KEEPALIVE
	tcpKeepIntvl = 0x101
	tcpKeepCnt   = 0x102
)

func readSockaddr(p, n uintptr) (addr, uintptr) {
	if n < 16 || p == 0 {
		return addr{}, uintptr(syscall.EINVAL)
	}
	b := mem(p, 16)
	if b[1] != syscall.AF_INET {
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
	b[0], b[1] = 16, syscall.AF_INET
	binary.BigEndian.PutUint16(b[2:], a.port)
	copy(b[4:8], a.ip[:])
	copy(mem(p, uintptr(max(n, 0))), b[:])
	putInt32(lenp, 16)
}

// Socket options and control messages of UDP sockets. darwin has no
// SO_RCVBUFFORCE, SO_SNDBUFFORCE, or UDP_SEGMENT.
const (
	soRcvbufForce = 0xffff
	soSndbufForce = 0xffff
	ipTOS         = syscall.IP_TOS
	ipRecvTOS     = 0x1b // IP_RECVTOS
	ipPktinfo     = 0x1a // IP_PKTINFO
	ipRecvPktinfo = 0x1a // IP_RECVPKTINFO
	udpSegment    = -1
)
