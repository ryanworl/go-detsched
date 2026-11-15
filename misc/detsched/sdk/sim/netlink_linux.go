// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package sim

import (
	"encoding/binary"
	"syscall"
)

// Netlink route sockets, which a node opens to list its interfaces, their
// addresses, and its routes (net.Interfaces, net.InterfaceAddrs, and
// route libraries such as go-netroute do, with syscall.NetlinkRIB). A node
// has two interfaces: lo, with 127.0.0.1/8, and eth0, with the node's
// address in 10.0.0.0/8, which is its only route. The socket answers each
// dump request (RTM_GETLINK, RTM_GETADDR, RTM_GETROUTE) in one datagram,
// which the next recvfrom returns; other requests get EOPNOTSUPP.

const afNetlink = syscall.AF_NETLINK

func (w *World) netlinkSocket(inc *incarnation, a [6]uintptr) (*socket, uintptr) {
	if t := a[1] & 0xf; t != syscall.SOCK_RAW && t != syscall.SOCK_DGRAM || a[2] != syscall.NETLINK_ROUTE {
		return nil, uintptr(syscall.EPROTONOSUPPORT)
	}
	return &socket{inc: inc, nl: &nlState{}}, 0
}

func (w *World) netlinkCall(d *fdesc, trap uintptr, a [6]uintptr) (uintptr, uintptr) {
	s := d.sock
	switch trap {
	case sysClose:
		delete(w.fds, int(a[0]))
		w.sockClose(s)
		return 0, 0
	case sysFcntl:
		switch a[1] {
		case syscall.F_GETFL:
			return syscall.O_RDWR, 0
		case syscall.F_SETFL, syscall.F_GETFD, syscall.F_SETFD:
			return 0, 0
		}
		return 0, uintptr(syscall.EINVAL)
	case sysBind, sysSetsockopt:
		return 0, 0
	case sysGetsockname:
		writeNetlinkAddr(a[1], a[2], uint32(s.inc.pid()))
		return 0, 0
	case sysSendto:
		b := mem(a[1], a[2])
		if len(b) < syscall.NLMSG_HDRLEN {
			return 0, uintptr(syscall.EINVAL)
		}
		s.nl.q = append(s.nl.q, w.netlinkReply(s.inc, b))
		return a[2], 0
	case sysRecvfrom:
		if len(s.nl.q) == 0 {
			return 0, uintptr(syscall.EAGAIN)
		}
		m := s.nl.q[0]
		s.nl.q = s.nl.q[1:]
		n := copy(mem(a[1], a[2]), m)
		writeNetlinkAddr(a[4], a[5], 0)
		return uintptr(n), 0
	}
	return 0, uintptr(syscall.EOPNOTSUPP)
}

func writeNetlinkAddr(p, lenp uintptr, pid uint32) {
	if p == 0 || lenp == 0 {
		return
	}
	var b [syscall.SizeofSockaddrNetlink]byte
	binary.LittleEndian.PutUint16(b[:], syscall.AF_NETLINK)
	binary.LittleEndian.PutUint32(b[4:], pid)
	copy(mem(p, uintptr(max(getInt32(lenp), 0))), b[:])
	putInt32(lenp, syscall.SizeofSockaddrNetlink)
}

// netlinkReply returns the reply to the request in b.
func (w *World) netlinkReply(inc *incarnation, b []byte) []byte {
	typ := binary.LittleEndian.Uint16(b[4:])
	seq := binary.LittleEndian.Uint32(b[8:])
	pid := uint32(inc.pid())
	ip := inc.node.ip
	var out []byte
	msg := func(typ uint16, body []byte) {
		h := make([]byte, syscall.NLMSG_HDRLEN, syscall.NLMSG_HDRLEN+len(body))
		binary.LittleEndian.PutUint32(h, uint32(syscall.NLMSG_HDRLEN+len(body)))
		binary.LittleEndian.PutUint16(h[4:], typ)
		binary.LittleEndian.PutUint16(h[6:], syscall.NLM_F_MULTI)
		binary.LittleEndian.PutUint32(h[8:], seq)
		binary.LittleEndian.PutUint32(h[12:], pid)
		out = append(out, nlAlign(append(h, body...))...)
	}
	type iface struct {
		name       string
		index      uint32
		arp, flags uint32
		mtu        uint32
		ip         [4]byte
		hw         []byte
	}
	ifs := []iface{
		{"lo", 1, syscall.ARPHRD_LOOPBACK, syscall.IFF_UP | syscall.IFF_LOOPBACK | syscall.IFF_RUNNING, 65536, [4]byte{127, 0, 0, 1}, make([]byte, 6)},
		{"eth0", 2, syscall.ARPHRD_ETHER, syscall.IFF_UP | syscall.IFF_BROADCAST | syscall.IFF_RUNNING | syscall.IFF_MULTICAST, 1500, ip, []byte{2, 0, ip[0], ip[1], ip[2], ip[3]}},
	}
	switch typ {
	case syscall.RTM_GETLINK:
		for _, f := range ifs {
			body := make([]byte, syscall.SizeofIfInfomsg)
			binary.LittleEndian.PutUint16(body[2:], uint16(f.arp))
			binary.LittleEndian.PutUint32(body[4:], f.index)
			binary.LittleEndian.PutUint32(body[8:], f.flags)
			binary.LittleEndian.PutUint32(body[12:], 0xffffffff)
			body = nlAttr(body, syscall.IFLA_IFNAME, append([]byte(f.name), 0))
			body = nlAttr(body, syscall.IFLA_MTU, le32(f.mtu))
			body = nlAttr(body, syscall.IFLA_ADDRESS, f.hw)
			msg(syscall.RTM_NEWLINK, body)
		}
	case syscall.RTM_GETADDR:
		for _, f := range ifs {
			body := make([]byte, syscall.SizeofIfAddrmsg)
			body[0] = syscall.AF_INET
			body[1] = 8 // prefix length
			if f.index == 1 {
				body[3] = syscall.RT_SCOPE_HOST
			}
			binary.LittleEndian.PutUint32(body[4:], f.index)
			body = nlAttr(body, syscall.IFA_ADDRESS, f.ip[:])
			body = nlAttr(body, syscall.IFA_LOCAL, f.ip[:])
			body = nlAttr(body, syscall.IFA_LABEL, append([]byte(f.name), 0))
			msg(syscall.RTM_NEWADDR, body)
		}
	case syscall.RTM_GETROUTE:
		body := make([]byte, syscall.SizeofRtMsg)
		body[0] = syscall.AF_INET
		body[1] = 8 // destination length
		body[4] = syscall.RT_TABLE_MAIN
		body[5] = syscall.RTPROT_KERNEL
		body[6] = syscall.RT_SCOPE_LINK
		body[7] = syscall.RTN_UNICAST
		body = nlAttr(body, syscall.RTA_TABLE, le32(syscall.RT_TABLE_MAIN))
		body = nlAttr(body, syscall.RTA_DST, []byte{ip[0], 0, 0, 0})
		body = nlAttr(body, syscall.RTA_PREFSRC, ip[:])
		body = nlAttr(body, syscall.RTA_OIF, le32(2))
		msg(syscall.RTM_NEWROUTE, body)
	default:
		errno := -int32(syscall.EOPNOTSUPP)
		e := make([]byte, 4, 4+len(b))
		binary.LittleEndian.PutUint32(e, uint32(errno))
		msg(syscall.NLMSG_ERROR, append(e, b[:syscall.NLMSG_HDRLEN]...))
		return out
	}
	msg(syscall.NLMSG_DONE, le32(0))
	return out
}

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func nlAlign(b []byte) []byte {
	for len(b)%syscall.NLMSG_ALIGNTO != 0 {
		b = append(b, 0)
	}
	return b
}

// nlAttr appends a route attribute to b.
func nlAttr(b []byte, typ uint16, v []byte) []byte {
	b = binary.LittleEndian.AppendUint16(b, uint16(syscall.SizeofRtAttr+len(v)))
	b = binary.LittleEndian.AppendUint16(b, typ)
	return nlAlign(append(b, v...))
}
