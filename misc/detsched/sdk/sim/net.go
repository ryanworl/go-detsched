// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (linux && (amd64 || arm64)) || (darwin && !ios && arm64)

package sim

import (
	"cmp"
	"slices"
	"syscall"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/buggify"
)

// The simulated network: TCP over IPv4.
//
// A write on a connection becomes segments of random sizes up to an MSS,
// sent back to back, that arrive after a latency drawn from the fault
// stream, never before an earlier segment on the same connection; so a
// reader may get a message in pieces. A writer can have sendBuffer bytes
// in flight on a connection, and sendBuffer+recvBuffer bytes in flight or
// arrived but unread, as TCP's windows allow; beyond that writes are
// short, or fail with EAGAIN until segments arrive or the reader reads. A segment that would cross a blocked link, or reach
// a node that is down, is held and retried when links heal; if it is still
// held after tcpTimeout, the sender's end fails with ETIMEDOUT, as
// TCP gives up retransmitting, and the other end learns nothing until it
// sends. A segment that reaches an end that no longer exists, of a closed
// socket, a crashed process, or an earlier incarnation of a node whose
// kernel is up again, is answered with RST, which resets the sender's end.
// An end that is reset or times out reports the error once, after the data
// that had arrived, to its next read or write; then reads see EOF and
// writes EPIPE, as on Linux.
//
// TCP keepalive works as on Linux: on a socket with SO_KEEPALIVE, after
// TCP_KEEPIDLE (default 2h) without traffic either way, the kernel sends
// a probe, which a peer that no longer has the connection answers with
// RST, and which, if it cannot get through for TCP_KEEPCNT times
// TCP_KEEPINTVL (defaults 9 and 75s), fails the connection with
// ETIMEDOUT. Package net turns it on, with 15s for both times, for every
// connection it dials or accepts, so a peer that lost power is found out
// even by an end that never sends.
//
// Buggify sites: "sim.net.retransmit" adds a retransmission delay of
// 200ms to 1s to some segments, and "sim.net.reset" resets some connections
// on a write.

const (
	sNew = iota
	sBound
	sListen
	sConnecting
	sConnected
	sClosed
)

type addr struct {
	ip   [4]byte
	port uint16
}

type socket struct {
	fd      int
	inc     *incarnation
	state   int
	local   addr
	remote  addr
	backlog []*socket // listener: connections not yet accepted
	soErr   uintptr   // pending SO_ERROR, for a connect in progress
	c       *conn
	side    int  // which end of c: 0 connected, 1 accepted
	dead    bool // its node lost power: it neither reads nor answers
	rdShut  bool
	ephem   bool      // connect bound it to an ephemeral port
	udp     *udpState // a datagram socket's state, or nil for TCP
	nl      *nlState  // a netlink route socket's state (netlink_linux.go)

	ka              bool  // SO_KEEPALIVE
	kaIdle, kaIntvl int64 // TCP_KEEPIDLE and TCP_KEEPINTVL, in ns, or 0 for the default
	kaCnt           int   // TCP_KEEPCNT, or 0 for the default
	kaGen           int   // invalidates earlier keepalive timers
}

// nlState is a netlink route socket's replies not yet read.
type nlState struct {
	q [][]byte
}

// Linux's keepalive defaults.
const (
	defKeepIdle  = 7200e9
	defKeepIntvl = 75e9
	defKeepCnt   = 9
)

// conn is an established connection; dir[i] carries data from end i.
type conn struct {
	id   uint64
	ends [2]*socket
	dir  [2]*stream
	err  [2]uintptr // the error end i reports next, once
	gone [2]bool    // end i was reset or timed out: it sends nothing more
	seen [2]int64   // when end i last sent, or heard from the other end
}

const (
	mss        = 1448    // largest segment
	sendBuffer = 4 << 20 // most bytes in flight in one direction
	recvBuffer = 6 << 20 // most bytes arrived but unread (Linux's tcp_rmem max)
)

// room is how many more bytes a writer may put in flight on out.
func (out *stream) room() int {
	return min(sendBuffer-out.inflight, sendBuffer+recvBuffer-out.inflight-out.unread)
}

type stream struct {
	segs     []seg    // in flight
	inflight int      // bytes in segs
	blocked  bool     // a write found no room
	rbuf     [][]byte // arrived, not yet read: segments' data, the first partly read
	unread   int      // bytes in rbuf
	fin      bool     // the sender's FIN arrived
	wclosed  bool     // the sender shut down writing
	last     int64    // arrival time of the last segment sent
	held     bool     // the head segment is held
	heldFrom int64    // when the head segment was first dropped, or 0
}

type seg struct {
	data  []byte
	fin   bool
	probe bool // a keepalive probe
	at    int64
}

// latency returns the delay of one message from a to b: the link's
// baseline latency and jitter, retransmissions of lost packets, and the
// added latency of Slowed faults.
func (w *World) latency(a, b *Node) int64 {
	l := w.link(a, b)
	d := int64(l.Latency)
	d += int64(expRand(l.Jitter))
	if l.Drop > 0 {
		for rto := int64(200e6); rto < int64(tcpTimeout) && chance(l.Drop); rto *= 2 {
			d += rto
		}
	}
	if a != b && !calm() && buggify.OnChance("sim.net.retransmit", 0.05) {
		d += 200e6 + int64(runtime_rand()%800e6)
	}
	for _, e := range w.linkState(a, b).slow {
		d += normal(e.Mean, e.Dev)
	}
	return d
}

// nodeAt returns the node that owns ip as seen from src, or nil.
func (w *World) nodeAt(src *Node, ip [4]byte) *Node {
	if ip[0] == 127 || ip == [4]byte{} {
		return src
	}
	return w.byIP[ip]
}

func (w *World) socketCall(inc *incarnation, trap uintptr, a [6]uintptr) (uintptr, uintptr) {
	if !inc.alive {
		return 0, uintptr(syscall.EIO)
	}
	if trap == sysSocketpair {
		return 0, uintptr(syscall.EOPNOTSUPP)
	}
	if a[0] == afNetlink {
		s, errno := w.netlinkSocket(inc, a)
		if errno != 0 {
			return 0, errno
		}
		inc.socks[s] = true
		return uintptr(w.newFD(&fdesc{inc: inc, sock: s})), 0
	}
	if a[0] != syscall.AF_INET {
		return 0, uintptr(syscall.EAFNOSUPPORT)
	}
	s := &socket{inc: inc}
	switch {
	case a[1]&0xf == syscall.SOCK_STREAM:
	case a[1]&0xf == syscall.SOCK_DGRAM && (a[2] == 0 || a[2] == syscall.IPPROTO_UDP):
		s.udp = newUDP()
	default:
		return 0, uintptr(syscall.EPROTONOSUPPORT)
	}
	inc.socks[s] = true
	return uintptr(w.newFD(&fdesc{inc: inc, sock: s})), 0
}

// ephemeral returns a port free in ports, one of inc's port maps, from
// the ephemeral range, or 0 if all are in use.
func (inc *incarnation) ephemeral(ports map[uint16]*socket) uint16 {
	for range 1 << 15 {
		p := inc.nextEp
		inc.nextEp++
		if inc.nextEp == 0 {
			inc.nextEp = 32768
		}
		if ports[p] == nil {
			return p
		}
	}
	return 0
}

// sockCall handles the system calls on a socket descriptor.
func (w *World) sockCall(d *fdesc, trap uintptr, a [6]uintptr) (uintptr, uintptr) {
	s := d.sock
	if s.udp != nil {
		return w.udpCall(d, trap, a)
	}
	if s.nl != nil {
		return w.netlinkCall(d, trap, a)
	}
	inc := s.inc
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
		if a[4] >= 4 {
			v := int64(getInt32(a[3]))
			switch {
			case a[1] == syscall.SOL_SOCKET && a[2] == syscall.SO_KEEPALIVE:
				s.ka = v != 0
			case a[1] == syscall.IPPROTO_TCP && a[2] == tcpKeepIdle && v > 0:
				s.kaIdle = v * 1e9
			case a[1] == syscall.IPPROTO_TCP && a[2] == tcpKeepIntvl && v > 0:
				s.kaIntvl = v * 1e9
			case a[1] == syscall.IPPROTO_TCP && a[2] == tcpKeepCnt && v > 0:
				s.kaCnt = int(v)
			default:
				return 0, 0
			}
			w.armKeepalive(s)
		}
		return 0, 0

	case sysGetsockopt:
		v := int32(0)
		if a[1] == syscall.SOL_SOCKET && a[2] == syscall.SO_ERROR {
			v = int32(s.soErr)
			s.soErr = 0
		} else if a[1] == syscall.SOL_SOCKET && a[2] == syscall.SO_TYPE {
			v = syscall.SOCK_STREAM
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
		if la.ip != inc.node.ip && la.ip != [4]byte{} && la.ip[0] != 127 {
			return 0, uintptr(syscall.EADDRNOTAVAIL)
		}
		if la.port == 0 {
			if la.port = inc.ephemeral(inc.ports); la.port == 0 {
				return 0, uintptr(syscall.EADDRINUSE)
			}
		} else if inc.ports[la.port] != nil {
			return 0, uintptr(syscall.EADDRINUSE)
		}
		s.local, s.state = la, sBound
		inc.ports[la.port] = s
		return 0, 0

	case sysListen:
		if s.state == sNew {
			p := inc.ephemeral(inc.ports)
			if p == 0 {
				return 0, uintptr(syscall.EADDRINUSE)
			}
			s.local = addr{port: p}
			inc.ports[s.local.port] = s
		} else if s.state != sBound {
			return 0, uintptr(syscall.EINVAL)
		}
		s.state = sListen
		return 0, 0

	case sysAccept4:
		if s.state != sListen {
			return 0, uintptr(syscall.EINVAL)
		}
		if len(s.backlog) == 0 {
			return 0, uintptr(syscall.EAGAIN)
		}
		c := s.backlog[0]
		s.backlog = s.backlog[1:]
		fd := w.newFD(&fdesc{inc: inc, sock: c})
		writeSockaddr(a[1], a[2], c.remote)
		return uintptr(fd), 0

	case sysConnect:
		return w.connect(s, a)

	case sysGetsockname:
		writeSockaddr(a[1], a[2], s.local)
		return 0, 0

	case sysGetpeername:
		if s.state != sConnected {
			return 0, uintptr(syscall.ENOTCONN)
		}
		writeSockaddr(a[1], a[2], s.remote)
		return 0, 0

	case sysRead, sysReadv, sysRecvfrom:
		var bufs [][]byte
		if trap == sysReadv {
			bufs = iovecs(a[1], a[2])
		} else {
			bufs = [][]byte{mem(a[1], a[2])}
		}
		return w.sockRead(s, bufs)

	case sysWrite, sysWritev, sysSendto:
		var bufs [][]byte
		if trap == sysWritev {
			bufs = iovecs(a[1], a[2])
		} else {
			bufs = [][]byte{mem(a[1], a[2])}
		}
		return w.sockWrite(s, bufs)

	case sysShutdown:
		if s.state != sConnected {
			return 0, uintptr(syscall.ENOTCONN)
		}
		if a[1] == syscall.SHUT_RD || a[1] == syscall.SHUT_RDWR {
			s.rdShut = true
			runtime_ready(uintptr(s.fd), 'r')
		}
		if a[1] == syscall.SHUT_WR || a[1] == syscall.SHUT_RDWR {
			w.sendFIN(s)
		}
		return 0, 0
	}
	return 0, eNOSYS
}

func (w *World) connect(s *socket, a [6]uintptr) (uintptr, uintptr) {
	switch s.state {
	case sConnecting:
		return 0, uintptr(syscall.EALREADY)
	case sConnected:
		return 0, uintptr(syscall.EISCONN)
	case sListen, sClosed:
		return 0, uintptr(syscall.EINVAL)
	}
	ra, e := readSockaddr(a[1], a[2])
	if e != 0 {
		return 0, e
	}
	inc := s.inc
	src := inc.node
	if s.state == sNew {
		ip := src.ip
		if ra.ip[0] == 127 || ra.ip == [4]byte{} {
			ip = [4]byte{127, 0, 0, 1}
		}
		p := inc.ephemeral(inc.ports)
		if p == 0 {
			return 0, uintptr(syscall.EADDRNOTAVAIL)
		}
		s.local, s.ephem = addr{ip, p}, true
		inc.ports[s.local.port] = s
	} else if s.local.ip == [4]byte{} {
		s.local.ip = src.ip
	}
	s.remote, s.state = ra, sConnecting
	dst := w.nodeAt(src, ra.ip)
	fail := func(errno uintptr) {
		if s.state == sConnecting {
			s.state, s.soErr = sClosed, errno
			if s.ephem && inc.ports[s.local.port] == s {
				delete(inc.ports, s.local.port) // as Linux releases it
			}
			runtime_ready(uintptr(s.fd), 'w')
		}
	}
	// The connection attempt fails after connectTimeout unless it
	// completes first.
	w.event(int64(connectTimeout), func() { fail(uintptr(syscall.ETIMEDOUT)) })
	// The SYN arrives after one latency. If it cannot arrive, it waits
	// for a link or node to change, as retransmitted SYNs would.
	var syn func()
	syn = func() {
		if s.state != sConnecting || !inc.alive {
			return
		}
		if dst == nil || dst.inc == nil && !dst.kernel || !w.linkUp(src, dst) || !w.linkUp(dst, src) {
			if dst != nil && !dst.removed {
				w.waiting = append(w.waiting, func() { w.event(w.latency(src, dst), syn) })
			}
			return
		}
		var l *socket
		if dst.inc != nil {
			l = dst.inc.ports[ra.port]
		}
		if l != nil && (l.state != sListen || l.local.ip != ra.ip && l.local.ip != [4]byte{} && !(dst == src && ra.ip[0] == 127)) {
			l = nil
		}
		if l == nil {
			// No listener, or no process on a node whose kernel is up:
			// RST comes back after another latency.
			w.event(w.latency(dst, src), func() { fail(uintptr(syscall.ECONNREFUSED)) })
			return
		}
		w.nextConn++
		c := &conn{id: w.nextConn, dir: [2]*stream{{}, {}}, seen: [2]int64{now(), now()}}
		srv := &socket{inc: dst.inc, state: sConnected, local: addr{ra.ip, ra.port}, remote: s.local, c: c, side: 1}
		if srv.local.ip == [4]byte{} {
			srv.local.ip = dst.ip
		}
		dst.inc.socks[srv] = true
		c.ends = [2]*socket{s, srv}
		w.allConns[c] = true
		l.backlog = append(l.backlog, srv)
		runtime_ready(uintptr(l.fd), 'r')
		// The SYN-ACK arrives back after another latency.
		w.event(w.latency(dst, src), func() {
			switch {
			case s.dead:
				// The client lost power: nothing answers, and the
				// server's end learns of it only when it sends.
			case c.gone[0]:
				// The listener closed and reset the connection.
				fail(uintptr(syscall.ECONNRESET))
			case s.state != sConnecting:
				// The client's kernel no longer knows the connection.
				w.event(w.latency(src, dst), func() { w.resetEnd(c, 1, uintptr(syscall.ECONNRESET)) })
			default:
				s.state, s.c, s.side = sConnected, c, 0
				runtime_ready(uintptr(s.fd), 'w')
			}
		})
	}
	if dst != nil { // with no host at the address, nothing answers
		w.event(w.latency(src, dst), syn)
	}
	return 0, uintptr(syscall.EINPROGRESS)
}

func (w *World) sockRead(s *socket, bufs [][]byte) (uintptr, uintptr) {
	switch s.state {
	case sConnecting:
		return 0, uintptr(syscall.EAGAIN)
	case sConnected:
	default:
		return 0, uintptr(syscall.ENOTCONN)
	}
	c := s.c
	in := c.dir[1-s.side]
	if in.unread == 0 {
		switch {
		case c.err[s.side] != 0:
			e := c.err[s.side]
			c.err[s.side] = 0
			return 0, e
		case in.fin || s.rdShut || c.gone[s.side]:
			return 0, 0
		}
		return 0, uintptr(syscall.EAGAIN)
	}
	total := 0
	for _, b := range bufs {
		for len(b) > 0 && len(in.rbuf) > 0 {
			m := copy(b, in.rbuf[0])
			b = b[m:]
			total += m
			if in.rbuf[0] = in.rbuf[0][m:]; len(in.rbuf[0]) == 0 {
				in.rbuf[0] = nil
				in.rbuf = in.rbuf[1:]
			}
		}
	}
	in.unread -= total
	if in.blocked && in.room() > 0 {
		in.blocked = false
		if src := c.ends[1-s.side]; src.fd != 0 {
			runtime_ready(uintptr(src.fd), 'w')
		}
	}
	return uintptr(total), 0
}

func (w *World) sockWrite(s *socket, bufs [][]byte) (uintptr, uintptr) {
	switch s.state {
	case sConnecting:
		return 0, uintptr(syscall.EAGAIN)
	case sConnected:
	default:
		return 0, uintptr(syscall.EPIPE)
	}
	c := s.c
	if e := c.err[s.side]; e != 0 {
		c.err[s.side] = 0
		return 0, e
	}
	out := c.dir[s.side]
	if c.gone[s.side] || out.wclosed {
		return 0, uintptr(syscall.EPIPE)
	}
	size := 0
	for _, b := range bufs {
		size += len(b)
	}
	if size == 0 {
		return 0, 0
	}
	room := out.room()
	if room <= 0 {
		out.blocked = true
		return 0, uintptr(syscall.EAGAIN)
	}
	// Appending to nil copies without first zeroing the new memory,
	// which make would do.
	n := min(size, room)
	var data []byte
	for _, b := range bufs {
		data = append(data, b[:min(len(b), n-len(data))]...)
	}
	if !calm() && buggify.OnChance("sim.net.reset", 0.001) {
		w.resetEnd(c, s.side, 0)
		w.resetEnd(c, 1-s.side, uintptr(syscall.ECONNRESET))
		return 0, uintptr(syscall.ECONNRESET)
	}
	w.send(c, s.side, data, false)
	return uintptr(len(data)), 0
}

// send puts data, or a FIN, in flight from end i of c: segments of random
// sizes up to an MSS, sent back to back at 1Gbit/s, which arrive after
// one latency.
func (w *World) send(c *conn, i int, data []byte, fin bool) {
	out := c.dir[i]
	c.seen[i] = now()
	at := w.grid(now() + w.latency(c.ends[i].inc.node, c.ends[1-i].inc.node))
	pumpAt := int64(-1)
	for first := true; first || len(data) > 0; first = false {
		sg := seg{fin: fin}
		if len(data) > 0 {
			n := min(len(data), 1+int(runtime_rand()%mss))
			sg.data, data = data[:n:n], data[n:]
			out.inflight += n
		}
		sg.at = max(at, out.last)
		out.last = sg.at
		out.segs = append(out.segs, sg)
		// One pump delivers every segment due by then.
		if t := w.grid(sg.at); t != pumpAt {
			pumpAt = t
			w.event(sg.at-now(), func() { w.pump(c, i) })
		}
		at += int64(len(sg.data)) * 8 // ns at 1Gbit/s
	}
}

// armKeepalive (re)starts the keepalive timer of s after its keepalive
// settings change.
func (w *World) armKeepalive(s *socket) {
	s.kaGen++
	if s.ka {
		gen := s.kaGen
		w.event(cmp.Or(s.kaIdle, defKeepIdle), func() { w.keepalive(s, gen) })
	}
}

// keepalive sends a keepalive probe on s if its connection has been idle
// for TCP_KEEPIDLE, and checks again after that long.
func (w *World) keepalive(s *socket, gen int) {
	if gen != s.kaGen || !s.ka || s.udp != nil || s.dead || !s.inc.alive || s.state != sConnected && s.state != sConnecting {
		return
	}
	idle := cmp.Or(s.kaIdle, defKeepIdle)
	next := idle
	if c := s.c; s.state == sConnected {
		i := s.side
		if c.gone[i] {
			return
		}
		out := c.dir[i]
		switch quiet := now() - c.seen[i]; {
		case len(out.segs) > 0:
			// Unacknowledged data: retransmissions find out.
		case quiet < idle:
			next = idle - quiet
		default:
			c.seen[i] = now()
			at := w.grid(now() + w.latency(s.inc.node, c.ends[1-i].inc.node))
			at = max(at, out.last)
			out.last = at
			out.segs = append(out.segs, seg{probe: true, at: at})
			w.event(at-now(), func() { w.pump(c, i) })
		}
	}
	w.event(next, func() { w.keepalive(s, gen) })
}

func (w *World) sendFIN(s *socket) {
	c := s.c
	if c == nil || c.dir[s.side].wclosed || c.gone[s.side] {
		return
	}
	c.dir[s.side].wclosed = true
	w.send(c, s.side, nil, true)
}

// pump delivers the segments of c's stream from end i that are due, in
// order, and holds them while they cannot arrive.
func (w *World) pump(c *conn, i int) {
	out := c.dir[i]
	for len(out.segs) > 0 && out.segs[0].at <= now() && !c.gone[i] {
		src, dst := c.ends[i], c.ends[1-i]
		a, b := src.inc.node, dst.inc.node
		st := w.linkState(a, b)
		up := !st.stopped && !st.jammed
		if (dst.state == sClosed || c.gone[1-i]) && !dst.dead && up {
			// A close, a process crash, or an end that was reset or
			// timed out: the kernel answers with RST.
			w.event(w.latency(b, a), func() { w.resetEnd(c, i, uintptr(syscall.ECONNRESET)) })
			return
		}
		if dst.dead && b.kernel && up {
			// The node's kernel is up again and has never heard of the
			// connection.
			w.event(w.latency(b, a), func() { w.resetEnd(c, i, uintptr(syscall.ECONNRESET)) })
			return
		}
		if dst.dead || !up {
			// Held. TCP gives up on a connection whose packets are
			// dropped for tcpTimeout, but not on one that is jammed.
			out.held = true
			if st.jammed && !st.stopped && !dst.dead {
				out.heldFrom = 0
			} else if out.heldFrom == 0 {
				out.heldFrom = now()
				from := out.heldFrom
				timeout := int64(tcpTimeout)
				if out.segs[0].probe {
					timeout = int64(cmp.Or(src.kaCnt, defKeepCnt)) * cmp.Or(src.kaIntvl, defKeepIntvl)
				}
				w.event(timeout, func() {
					if out.heldFrom == from && len(out.segs) > 0 {
						w.resetEnd(c, i, uintptr(syscall.ETIMEDOUT))
					}
				})
			}
			return
		}
		out.held, out.heldFrom = false, 0
		sg := out.segs[0]
		out.segs = out.segs[1:]
		out.inflight -= len(sg.data)
		c.seen[0], c.seen[1] = now(), now()
		if sg.probe {
			continue
		}
		if sg.fin {
			out.fin = true
		} else {
			// Segments' data is never written again: keep it.
			out.rbuf = append(out.rbuf, sg.data)
			out.unread += len(sg.data)
		}
		if dst.fd != 0 {
			runtime_ready(uintptr(dst.fd), 'r')
		}
		if out.blocked && out.room() > 0 {
			out.blocked = false
			if src.fd != 0 {
				runtime_ready(uintptr(src.fd), 'w')
			}
		}
	}
}

// netChanged retries held segments and waiting connection attempts after
// links heal or a node starts.
func (w *World) netChanged() {
	for _, c := range w.conns() {
		for i, out := range c.dir {
			if !out.held || len(out.segs) == 0 {
				continue
			}
			a, b := c.ends[i].inc.node, c.ends[1-i].inc.node
			if !w.linkUp(a, b) {
				// Still held, but perhaps for another reason.
				w.pump(c, i)
				continue
			}
			out.held = false
			at := w.grid(now() + w.latency(a, b))
			for j := range out.segs {
				out.segs[j].at = max(out.segs[j].at, at)
			}
			out.last = max(out.last, at)
			w.event(at-now(), func() { w.pump(c, i) })
		}
	}
	w.udpNetChanged()
	waiting := w.waiting
	w.waiting = nil
	for _, f := range waiting {
		f()
	}
}

// conns returns the connections, in the order they were made.
func (w *World) conns() []*conn {
	cs := make([]*conn, 0, len(w.allConns))
	for c := range w.allConns {
		cs = append(cs, c)
	}
	slices.SortFunc(cs, func(a, b *conn) int { return cmp.Compare(a.id, b.id) })
	return cs
}

// resetEnd ends end i of c, which reports errno (if not 0) once: an RST
// arrived, or TCP gave up retransmitting. The end drops what it had not
// yet delivered, and keeps the data that had arrived for it to read.
func (w *World) resetEnd(c *conn, i int, errno uintptr) {
	if c.gone[i] {
		return
	}
	c.gone[i], c.err[i] = true, errno
	c.dir[i].drop()
	if c.gone[1-i] {
		delete(w.allConns, c)
	}
	if s := c.ends[i]; s.fd != 0 && s.c == c {
		runtime_ready(uintptr(s.fd), 'r')
		runtime_ready(uintptr(s.fd), 'w')
	}
}

// drop discards the segments in flight.
func (out *stream) drop() {
	out.segs, out.inflight, out.held, out.heldFrom = nil, 0, false, 0
}

func (w *World) sockClose(s *socket) {
	inc := s.inc
	if s.nl != nil {
		s.nl.q = nil
		s.state, s.fd = sClosed, 0
		delete(inc.socks, s)
		return
	}
	if s.udp != nil {
		if inc.uports[s.local.port] == s {
			delete(inc.uports, s.local.port)
		}
		s.udp.rq, s.udp.rqBytes = nil, 0
		s.state, s.fd = sClosed, 0
		delete(inc.socks, s)
		return
	}
	switch s.state {
	case sListen:
		// Connections not yet accepted are reset.
		for _, srv := range s.backlog {
			c := srv.c
			c.gone[1] = true
			c.dir[1].drop()
			srv.state = sClosed
			delete(inc.socks, srv)
			w.event(w.latency(inc.node, c.ends[0].inc.node), func() { w.resetEnd(c, 0, uintptr(syscall.ECONNRESET)) })
		}
		s.backlog = nil
	case sConnected:
		c, i := s.c, s.side
		switch {
		case c.gone[i]:
		case c.dir[1-i].unread > 0:
			// Closing with unread data sends RST instead of what is left
			// to send.
			c.gone[i] = true
			c.dir[i].drop()
			w.event(w.latency(inc.node, c.ends[1-i].inc.node), func() { w.resetEnd(c, 1-i, uintptr(syscall.ECONNRESET)) })
		default:
			w.sendFIN(s)
		}
	}
	if inc.ports[s.local.port] == s {
		delete(inc.ports, s.local.port)
	}
	s.state = sClosed
	s.fd = 0
	delete(inc.socks, s)
}
