// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (linux && (amd64 || arm64)) || (darwin && !ios && arm64)

package sim

import (
	"syscall"
	"unsafe"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/buggify"
)

// The simulated network: UDP over IPv4.
//
// Each datagram travels on its own: it arrives after a latency drawn from
// the fault stream, so datagrams overtake each other, and it is lost with
// the link's Drop probability. A datagram longer than the link's MTU
// (Link.MTU, default 1500 bytes of IP packet) is lost, as with the Don't
// Fragment bit set; one longer than the sender's interface MTU fails with
// EMSGSIZE. A Stopped fault drops datagrams, also those in flight when it
// starts, a Jammed one holds them until it ends, and a Slowed one delays
// them. A datagram that reaches a node
// that is down, or a port nothing is bound to, is lost, and so is one that
// finds the receiver's buffer (SO_RCVBUF) full. A node behind a NAT
// (Node.SetNAT) sends from public ports, which Node.Rebind changes.
//
// Sockets support what QUIC stacks use on Linux: sendto, recvfrom,
// sendmsg, recvmsg, sendmmsg, and recvmmsg, connected sockets, the ECN bits
// of the TOS byte (IP_TOS on send, IP_RECVTOS on receive), IP_PKTINFO, and
// generic segmentation offload (UDP_SEGMENT).
//
// The network marks ECN-capable datagrams Congestion Experienced when the
// receiver's buffer is more than half full. Buggify sites: "sim.net.udpdrop"
// loses some datagrams, "sim.net.udpdup" delivers some twice,
// "sim.net.udpreorder" delays some by up to 50ms, "sim.net.ecnce" marks some
// ECN-capable datagrams Congestion Experienced, and, for every datagram of a
// run in which they are active, "sim.net.ecnbleach" clears the ECN bits and
// "sim.net.ecnmangle" turns ECT(0) into ECT(1).

const (
	ifaceMTU      = 1500
	udpHeader     = 28 // IPv4 and UDP headers
	rmemDefault   = 212992
	rmemMax       = 212992
	dgramOverhead = 256 // what Linux charges a datagram's buffer beyond its payload
)

// dgram is a datagram in flight or queued at a socket.
type dgram struct {
	data     []byte
	from, to addr
	tos      byte // the ECN bits
}

// udpState is a datagram socket's state.
type udpState struct {
	rq      []dgram // arrived, not yet read
	rqBytes int     // charged to the receive buffer
	rcvbuf  int     // SO_RCVBUF, as set
	sndbuf  int
	recvTOS bool // IP_RECVTOS
	pktinfo bool // IP_PKTINFO
}

func newUDP() *udpState { return &udpState{rcvbuf: rmemDefault, sndbuf: rmemDefault} }

// udpBind binds s to la, or to an ephemeral port if la's port is 0.
func (w *World) udpBind(s *socket, la addr) uintptr {
	inc := s.inc
	if la.ip != inc.node.ip && la.ip != [4]byte{} && la.ip[0] != 127 {
		return uintptr(syscall.EADDRNOTAVAIL)
	}
	if la.port == 0 {
		if la.port = inc.ephemeral(inc.uports); la.port == 0 {
			return uintptr(syscall.EADDRINUSE)
		}
	} else if inc.uports[la.port] != nil {
		return uintptr(syscall.EADDRINUSE)
	}
	s.local, s.state = la, sBound
	inc.uports[la.port] = s
	return 0
}

// udpCall handles the system calls on a datagram socket.
func (w *World) udpCall(d *fdesc, trap uintptr, a [6]uintptr) (uintptr, uintptr) {
	s := d.sock
	u := s.udp
	switch trap {
	case sysClose:
		delete(w.fds, int(a[0]))
		w.sockClose(s)
		return 0, 0

	case sysFcntl:
		switch a[1] {
		case syscall.F_GETFL:
			return syscall.O_RDWR | syscall.O_NONBLOCK, 0
		case syscall.F_SETFL, syscall.F_GETFD, syscall.F_SETFD:
			return 0, 0
		}
		return 0, uintptr(syscall.EINVAL)

	case sysSetsockopt:
		v := int(getInt32(a[3]))
		switch {
		case a[1] == syscall.SOL_SOCKET && a[2] == syscall.SO_RCVBUF:
			u.rcvbuf = min(max(v, 1024), rmemMax)
		case a[1] == syscall.SOL_SOCKET && a[2] == soRcvbufForce:
			u.rcvbuf = max(v, 1024)
		case a[1] == syscall.SOL_SOCKET && a[2] == syscall.SO_SNDBUF:
			u.sndbuf = min(max(v, 1024), rmemMax)
		case a[1] == syscall.SOL_SOCKET && a[2] == soSndbufForce:
			u.sndbuf = max(v, 1024)
		case a[1] == syscall.IPPROTO_IP && a[2] == ipRecvTOS:
			u.recvTOS = v != 0
		case a[1] == syscall.IPPROTO_IP && a[2] == ipRecvPktinfo:
			u.pktinfo = v != 0
		case a[1] == syscall.IPPROTO_IPV6:
			return 0, uintptr(syscall.ENOPROTOOPT) // an IPv4 socket
		}
		return 0, 0

	case sysGetsockopt:
		v := int32(0)
		switch {
		case a[1] == syscall.SOL_SOCKET && a[2] == syscall.SO_TYPE:
			v = syscall.SOCK_DGRAM
		case a[1] == syscall.SOL_SOCKET && a[2] == syscall.SO_RCVBUF:
			v = int32(2 * u.rcvbuf) // Linux reports the doubled value it uses
		case a[1] == syscall.SOL_SOCKET && a[2] == syscall.SO_SNDBUF:
			v = int32(2 * u.sndbuf)
		case a[1] == syscall.IPPROTO_UDP && int(a[2]) == udpSegment:
		case a[1] == syscall.SOL_SOCKET && a[2] == syscall.SO_ERROR:
		default:
			return 0, uintptr(syscall.ENOPROTOOPT)
		}
		if getInt32(a[4]) >= 4 {
			putInt32(a[3], v)
			putInt32(a[4], 4)
		}
		return 0, 0

	case sysBind:
		if s.state != sNew {
			return 0, uintptr(syscall.EINVAL)
		}
		la, e := readSockaddr(a[1], a[2])
		if e != 0 {
			return 0, e
		}
		return 0, w.udpBind(s, la)

	case sysConnect:
		ra, e := readSockaddr(a[1], a[2])
		if e != 0 {
			return 0, e
		}
		if s.state == sNew {
			la := addr{}
			if ra.ip[0] == 127 || ra.ip == [4]byte{} {
				la.ip = [4]byte{127, 0, 0, 1}
			}
			if e := w.udpBind(s, la); e != 0 {
				return 0, e
			}
		}
		s.remote, s.state = ra, sConnected
		return 0, 0

	case sysGetsockname:
		la := s.local
		if la.ip == [4]byte{} && s.state == sConnected {
			la.ip = s.inc.node.ip
		}
		writeSockaddr(a[1], a[2], la)
		return 0, 0

	case sysGetpeername:
		if s.state != sConnected {
			return 0, uintptr(syscall.ENOTCONN)
		}
		writeSockaddr(a[1], a[2], s.remote)
		return 0, 0

	case sysRead, sysRecvfrom:
		dg, ok := w.udpPop(s)
		if !ok {
			return 0, uintptr(syscall.EAGAIN)
		}
		n := copy(mem(a[1], a[2]), dg.data)
		if trap == sysRecvfrom {
			writeSockaddr(a[4], a[5], dg.from)
			if a[3]&syscall.MSG_TRUNC != 0 {
				n = len(dg.data)
			}
		}
		return uintptr(n), 0

	case sysReadv:
		dg, ok := w.udpPop(s)
		if !ok {
			return 0, uintptr(syscall.EAGAIN)
		}
		return uintptr(scatter(iovecs(a[1], a[2]), dg.data)), 0

	case sysWrite, sysWritev, sysSendto:
		var bufs [][]byte
		if trap == sysWritev {
			bufs = iovecs(a[1], a[2])
		} else {
			bufs = [][]byte{mem(a[1], a[2])}
		}
		to := s.remote
		if trap == sysSendto && a[4] != 0 {
			var e uintptr
			if to, e = readSockaddr(a[4], uintptr(uint32(a[5]))); e != 0 {
				return 0, e
			}
		} else if s.state != sConnected {
			return 0, uintptr(syscall.EDESTADDRREQ)
		}
		return w.udpSend(s, to, gather(bufs), 0, 0)

	case sysRecvmsg:
		return w.udpRecvmsg(s, (*syscall.Msghdr)(ptr(a[1])))

	case sysSendmsg:
		return w.udpSendmsg(s, (*syscall.Msghdr)(ptr(a[1])))

	case sysRecvmmsg, sysSendmmsg:
		// struct mmsghdr { struct msghdr msg_hdr; unsigned int msg_len; }
		size := (unsafe.Sizeof(syscall.Msghdr{}) + 4 + 7) &^ 7
		n := uintptr(0)
		for ; n < a[2]; n++ {
			p := a[1] + n*size
			var r, e uintptr
			if trap == sysRecvmmsg {
				r, e = w.udpRecvmsg(s, (*syscall.Msghdr)(ptr(p)))
			} else {
				r, e = w.udpSendmsg(s, (*syscall.Msghdr)(ptr(p)))
			}
			if e != 0 {
				if n == 0 {
					return 0, e
				}
				break
			}
			*(*uint32)(ptr(p + unsafe.Sizeof(syscall.Msghdr{}))) = uint32(r)
		}
		return n, 0

	case sysShutdown:
		if s.state != sConnected {
			return 0, uintptr(syscall.ENOTCONN)
		}
		return 0, 0
	}
	return 0, eNOSYS
}

func gather(bufs [][]byte) []byte {
	n := 0
	for _, b := range bufs {
		n += len(b)
	}
	data := make([]byte, 0, n)
	for _, b := range bufs {
		data = append(data, b...)
	}
	return data
}

func scatter(bufs [][]byte, data []byte) int {
	n := 0
	for _, b := range bufs {
		m := copy(b, data)
		data = data[m:]
		n += m
	}
	return n
}

// udpPop takes the next datagram from s's receive queue.
func (w *World) udpPop(s *socket) (dgram, bool) {
	u := s.udp
	if len(u.rq) == 0 {
		return dgram{}, false
	}
	dg := u.rq[0]
	u.rq[0] = dgram{}
	u.rq = u.rq[1:]
	u.rqBytes -= len(dg.data) + dgramOverhead
	return dg, true
}

func (w *World) udpRecvmsg(s *socket, m *syscall.Msghdr) (uintptr, uintptr) {
	dg, ok := w.udpPop(s)
	if !ok {
		return 0, uintptr(syscall.EAGAIN)
	}
	n := scatter(iovecs(uintptr(unsafe.Pointer(m.Iov)), uintptr(m.Iovlen)), dg.data)
	m.Flags = 0
	if n < len(dg.data) {
		m.Flags |= syscall.MSG_TRUNC
	}
	if m.Name != nil {
		writeSockaddr(uintptr(unsafe.Pointer(m.Name)), uintptr(unsafe.Pointer(&m.Namelen)), dg.from)
	}
	// Control messages: the TOS byte, and the address the datagram was
	// sent to.
	ctl := mem(uintptr(unsafe.Pointer(m.Control)), uintptr(m.Controllen))
	used := 0
	put := func(level, typ int32, data []byte) {
		space := syscall.CmsgSpace(len(data))
		if used+space > len(ctl) {
			m.Flags |= syscall.MSG_CTRUNC
			return
		}
		b := ctl[used : used+space]
		clear(b)
		h := (*syscall.Cmsghdr)(unsafe.Pointer(&b[0]))
		h.Level, h.Type = level, typ
		h.SetLen(syscall.CmsgLen(len(data)))
		copy(b[syscall.CmsgLen(0):], data)
		used += space
	}
	if s.udp.recvTOS {
		put(syscall.IPPROTO_IP, ipTOS, []byte{dg.tos})
	}
	if s.udp.pktinfo {
		// struct in_pktinfo { int ipi_ifindex; struct in_addr ipi_spec_dst, ipi_addr; }
		var pi [12]byte
		pi[0] = 1 // the node's only interface, in host byte order
		copy(pi[4:8], dg.to.ip[:])
		copy(pi[8:12], dg.to.ip[:])
		put(syscall.IPPROTO_IP, ipPktinfo, pi[:])
	}
	m.SetControllen(used)
	return uintptr(n), 0
}

func (w *World) udpSendmsg(s *socket, m *syscall.Msghdr) (uintptr, uintptr) {
	to := s.remote
	if m.Name != nil {
		var e uintptr
		if to, e = readSockaddr(uintptr(unsafe.Pointer(m.Name)), uintptr(m.Namelen)); e != 0 {
			return 0, e
		}
	} else if s.state != sConnected {
		return 0, uintptr(syscall.EDESTADDRREQ)
	}
	var tos byte
	var gso int
	ctl := mem(uintptr(unsafe.Pointer(m.Control)), uintptr(m.Controllen))
	for len(ctl) >= syscall.CmsgLen(0) {
		h := (*syscall.Cmsghdr)(unsafe.Pointer(&ctl[0]))
		l := int(h.Len)
		if l < syscall.CmsgLen(0) || l > len(ctl) {
			return 0, uintptr(syscall.EINVAL)
		}
		data := ctl[syscall.CmsgLen(0):l]
		switch {
		case h.Level == syscall.IPPROTO_IP && h.Type == ipTOS && len(data) >= 1:
			tos = data[0] // Linux takes a byte or an int; the low byte either way
		case h.Level == syscall.IPPROTO_UDP && h.Type == udpSegment && len(data) >= 2:
			gso = int(*(*uint16)(unsafe.Pointer(&data[0])))
		}
		ctl = ctl[min(syscall.CmsgSpace(l-syscall.CmsgLen(0)), len(ctl)):]
	}
	data := gather(iovecs(uintptr(unsafe.Pointer(m.Iov)), uintptr(m.Iovlen)))
	return w.udpSend(s, to, data, tos&3, gso)
}

// udpSend sends data from s to the address to, as one datagram or, with
// gso, as datagrams of gso bytes each, the last one shorter.
func (w *World) udpSend(s *socket, to addr, data []byte, tos byte, gso int) (uintptr, uintptr) {
	inc := s.inc
	if !inc.alive {
		return 0, uintptr(syscall.EIO)
	}
	if s.state == sNew {
		la := addr{}
		if to.ip[0] == 127 {
			la.ip = [4]byte{127, 0, 0, 1}
		}
		if e := w.udpBind(s, la); e != 0 {
			return 0, e
		}
	}
	if s.state == sClosed {
		return 0, uintptr(syscall.EBADF)
	}
	if gso <= 0 || gso >= len(data) {
		gso = len(data)
	}
	if gso+udpHeader > ifaceMTU || len(data) > 65507 {
		return 0, uintptr(syscall.EMSGSIZE)
	}
	from := s.local
	if from.ip == [4]byte{} {
		from.ip = inc.node.ip
		if to.ip[0] == 127 {
			from.ip = [4]byte{127, 0, 0, 1}
		}
	}
	src := inc.node
	dst := w.nodeAt(src, to.ip)
	if src.nat != nil && dst != src {
		from.port = src.natPort(from.port)
	}
	for off := 0; off < len(data) || off == 0; off += gso {
		seg := data[off:min(off+gso, len(data))]
		if dst != nil {
			w.udpRoute(src, dst, dgram{data: seg, from: from, to: to, tos: tos})
		}
		if len(data) == 0 {
			break
		}
	}
	return uintptr(len(data)), 0
}

// udpRoute puts dg in flight from src to dst, or loses it.
func (w *World) udpRoute(src, dst *Node, dg dgram) {
	if src != dst {
		l := w.link(src, dst)
		mtu := l.MTU
		if mtu == 0 {
			mtu = ifaceMTU
		}
		if len(dg.data)+udpHeader > mtu {
			return
		}
		if l.Drop > 0 && chance(l.Drop) {
			return
		}
		if fault("sim.net.udpdrop", "fault: sim.net.udpdrop", 0.05) {
			return
		}
		if dg.tos != 0 {
			switch {
			case buggify.Active("sim.net.ecnbleach"):
				dg.tos = 0
			case buggify.Active("sim.net.ecnmangle") && dg.tos == 2:
				dg.tos = 1
			}
		}
		if dg.tos != 0 && fault("sim.net.ecnce", "fault: sim.net.ecnce", 0.05) {
			dg.tos = 3
		}
		st := w.linkState(src, dst)
		if st.stopped {
			return
		}
		if st.jammed {
			w.udpHeld = append(w.udpHeld, heldDgram{src, dst, dg})
			return
		}
	}
	copies := 1
	if src != dst && fault("sim.net.udpdup", "fault: sim.net.udpdup", 0.02) {
		copies = 2
	}
	for range copies {
		d := w.udpLatency(src, dst)
		w.event(d, func() { w.udpLand(src, dst, dg) })
	}
}

// udpLand delivers dg at the end of its flight from src, unless a fault
// that started meanwhile drops or holds it, as a link that goes down
// loses the packets on it.
func (w *World) udpLand(src, dst *Node, dg dgram) {
	if src != dst {
		st := w.linkState(src, dst)
		if st.stopped {
			return
		}
		if st.jammed {
			w.udpHeld = append(w.udpHeld, heldDgram{src, dst, dg})
			return
		}
	}
	w.udpArrive(dst, dg)
}

type heldDgram struct {
	src, dst *Node
	dg       dgram
}

// udpLatency returns the delay of one datagram from a to b: the link's
// baseline latency and jitter, the added latency of Slowed faults, and
// now and then a reordering delay.
func (w *World) udpLatency(a, b *Node) int64 {
	l := w.link(a, b)
	d := int64(l.Latency)
	d += int64(expRand(l.Jitter))
	for _, e := range w.linkState(a, b).slow {
		d += normal(e.Mean, e.Dev)
	}
	if a != b && fault("sim.net.udpreorder", "fault: sim.net.udpreorder", 0.05) {
		d += int64(runtime_rand() % 50e6)
	}
	return d
}

// udpArrive delivers dg to the socket bound to its address on dst, if
// any, and if the socket has room for it.
func (w *World) udpArrive(dst *Node, dg dgram) {
	inc := dst.inc
	if inc == nil || !inc.alive {
		return
	}
	if dst.nat != nil && dg.from.ip != dst.ip && dg.from.ip[0] != 127 {
		p, ok := dst.natRev[dg.to.port]
		if !ok {
			return // the NAT has no such port
		}
		dg.to.port = p
	}
	s := inc.uports[dg.to.port]
	if s == nil || s.dead || s.state == sClosed {
		return
	}
	if s.local.ip != [4]byte{} && s.local.ip != dg.to.ip && !(dg.to.ip[0] == 127 && s.local.ip[0] == 127) {
		return
	}
	if s.state == sConnected && s.remote != dg.from {
		return
	}
	u := s.udp
	if u.rqBytes+len(dg.data)+dgramOverhead > 2*u.rcvbuf {
		return
	}
	if dg.tos == 1 || dg.tos == 2 {
		if u.rqBytes > u.rcvbuf {
			dg.tos = 3 // the queue builds up: Congestion Experienced
		}
	}
	u.rq = append(u.rq, dg)
	u.rqBytes += len(dg.data) + dgramOverhead
	if s.fd != 0 {
		runtime_ready(uintptr(s.fd), 'r')
	}
}

// udpNetChanged delivers held datagrams whose links are no longer jammed,
// and drops those whose links now drop them.
func (w *World) udpNetChanged() {
	held := w.udpHeld
	w.udpHeld = nil
	for _, h := range held {
		st := w.linkState(h.src, h.dst)
		switch {
		case st.jammed && !st.stopped:
			w.udpHeld = append(w.udpHeld, h)
		case st.stopped:
		default:
			src, dst, dg := h.src, h.dst, h.dg
			w.event(w.udpLatency(src, dst), func() { w.udpLand(src, dst, dg) })
		}
	}
}
