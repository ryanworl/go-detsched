// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/runtime/maps"
	"internal/runtime/sys"
	"unsafe"
)

// Pointer identities for GOEXPERIMENT=detsched.
//
// A map hashes its keys, and where a key lands decides the order in which
// range visits it. A pointer key (a pointer, channel or unsafe.Pointer,
// alone or in a struct, array or interface) would hash by its address,
// and addresses depend on the heap's layout, which differs between an
// optimized build and one with -N -l, and with anything that moves an
// allocation. In det mode such a key hashes by its pointer's identity
// instead (detptrhash): a number, in the order in which the program first
// hashed that pointer, which the pointer keeps until its object is freed.
// The compiler gives pointer-shaped keys a hash function that calls
// detptrhash (see hashSignature in cmd/compile/internal/reflectdata), the
// map fast paths for 4- and 8-byte keys call it for pointers, and
// typehash, which interfaces and reflect hash with, does too.
//
//   - A pointer into the heap is identified by its exact address, so that
//     two pointers into one object, such as two fields, differ, and so do
//     two tiny allocations that share a block. The sweeper drops the
//     identities of the pointers into the objects it frees
//     (detPtrIDSweep), before their memory can hold other objects.
//   - A pointer into a module's data, bss or read-only data, such as a
//     global or a type descriptor, keeps its identity for good.
//   - Any other pointer, such as one into a stack, which a map can only
//     hold through package unsafe, hashes by its address.
//
// Only a pointer that a map stores gets an identity (maps.DetAssign): a
// pointer without one is in no map, so a lookup of it hashes its address,
// which finds nothing just as well. An optimized build may make lookups
// that a build without optimizations does not, or the other way around,
// such as one whose key the other build keeps on the stack; they leave
// the identities that later keys get alone.
//
// Identities cost a probe of a hash table per hash, and the first hash of
// a pointer also records it under its span, for the sweeper. The tables
// take no lock: det mode runs all Go code on one P (see procresize), and
// the code that uses them holds its M (acquirem), so that nothing
// preempts it.

type detPtrIDEntry struct {
	p uintptr
	v uint64
}

// A detPtrTable is an open-addressed hash table keyed by address, with
// linear probing; an entry with p == 0 is free.
type detPtrTable struct {
	e     []detPtrIDEntry // its length is 1<<(64-shift), or 0
	shift uint
	n     int // entries in use
}

// A detPtrBlock holds pointers into one span that have identities.
type detPtrBlock struct {
	next *detPtrBlock
	n    uintptr
	p    [62]uintptr
}

var detPtrIDs struct {
	inited bool
	next   uint64 // the last identity given out

	ids   detPtrTable // the identities of pointers
	spans detPtrTable // by span base, its first *detPtrBlock
	alloc fixalloc    // of detPtrBlocks
}

// detptrhash is the hash function of a pointer-shaped key in det mode,
// which the compiler calls from the hash functions of such types: the
// hash of the identity of the pointer at p, with seed h.
func detptrhash(p unsafe.Pointer, h uintptr) uintptr {
	id := detPtrID(*(*uintptr)(p))
	return memhash64(noescape(unsafe.Pointer(&id)), h)
}

// detHashAssign is hash(key, seed) for a key the caller stores, for the
// hash tries of internal/sync and unique.
func detHashAssign(hash func(unsafe.Pointer, uintptr) uintptr, key unsafe.Pointer, seed uintptr) uintptr {
	maps.DetAssign = true
	h := hash(key, seed)
	maps.DetAssign = false
	return h
}

//go:linkname internal_sync_runtime_detHashAssign internal/sync.runtime_detHashAssign
func internal_sync_runtime_detHashAssign(hash func(unsafe.Pointer, uintptr) uintptr, key unsafe.Pointer, seed uintptr) uintptr {
	return detHashAssign(hash, key, seed)
}

//go:linkname unique_runtime_detHashAssign unique.runtime_detHashAssign
func unique_runtime_detHashAssign(hash func(unsafe.Pointer, uintptr) uintptr, key unsafe.Pointer, seed uintptr) uintptr {
	return detHashAssign(hash, key, seed)
}

// detTypeHash is typehash for a type t of regular memory that holds
// pointers, which hash by their identities.
func detTypeHash(t *_type, p unsafe.Pointer, h uintptr) uintptr {
	switch t.Kind() {
	case abi.Array:
		a := (*arraytype)(unsafe.Pointer(t))
		for i := uintptr(0); i < a.Len; i++ {
			h = typehash(a.Elem, add(p, i*a.Elem.Size_), h)
		}
		return h
	case abi.Struct:
		s := (*structtype)(unsafe.Pointer(t))
		for _, f := range s.Fields {
			if f.Name.IsBlank() {
				continue
			}
			h = typehash(f.Typ, add(p, f.Offset), h)
		}
		return h
	default: // a pointer, channel or unsafe.Pointer
		return detptrhash(p, h)
	}
}

// detPtrID returns the identity of pointer p (see above).
func detPtrID(p uintptr) uint64 {
	if p == 0 {
		return 0
	}
	d := &detPtrIDs
	mp := acquirem()
	if e := d.ids.find(p); e != nil {
		id := e.v
		releasem(mp)
		return id
	}
	if !maps.DetAssign {
		releasem(mp)
		return uint64(p) | 1<<63
	}
	if !d.inited {
		d.alloc.init(unsafe.Sizeof(detPtrBlock{}), nil, nil, &memstats.other_sys)
		d.inited = true
	}
	span := spanOfHeap(p)
	if span == nil {
		id := uint64(p) | 1<<63
		if detPtrInModule(p) {
			d.next++
			id = d.next
			d.ids.insert(p, id)
		}
		releasem(mp)
		return id
	}
	// The span must be swept before p is recorded under it: sweeping it
	// later would see p's object as freed if it was allocated since.
	// Sweeping it now may drop identities, but not p's, whose object
	// is live.
	span.ensureSwept()
	d.next++
	id := d.next
	d.ids.insert(p, id)
	var b *detPtrBlock
	if e := d.spans.find(span.base()); e != nil {
		b = (*detPtrBlock)(unsafe.Pointer(uintptr(e.v)))
	}
	if b == nil || b.n == uintptr(len(b.p)) {
		nb := (*detPtrBlock)(d.alloc.alloc())
		nb.next, nb.n = b, 0
		if b == nil {
			d.spans.insert(span.base(), uint64(uintptr(unsafe.Pointer(nb))))
		} else {
			d.spans.find(span.base()).v = uint64(uintptr(unsafe.Pointer(nb)))
		}
		b = nb
	}
	b.p[b.n] = p
	b.n++
	releasem(mp)
	return id
}

// detPtrInModule reports whether p points into a module's data, bss or
// read-only data.
func detPtrInModule(p uintptr) bool {
	for md := &firstmoduledata; md != nil; md = md.next {
		if md.noptrdata <= p && p < md.end || md.types <= p && p < md.etypes || md.rodata <= p && p < md.epclntab {
			return true
		}
	}
	return false
}

// detPtrIDSweep drops the identities of the pointers into the objects of
// span s that are not marked, which the sweeper is freeing. Called by
// sweep, after it has handled the specials.
func detPtrIDSweep(s *mspan) {
	d := &detPtrIDs
	e := d.spans.find(s.base())
	if e == nil {
		return
	}
	mp := acquirem()
	// Move the pointers that stay to the front of the list of blocks.
	head := (*detPtrBlock)(unsafe.Pointer(uintptr(e.v)))
	keep, kept := head, uintptr(0)
	for b := head; b != nil; b = b.next {
		for i := uintptr(0); i < b.n; i++ {
			p := b.p[i]
			if s.markBitsForIndex((p - s.base()) / s.elemsize).isMarked() {
				if kept == uintptr(len(keep.p)) {
					keep, kept = keep.next, 0
				}
				keep.p[kept] = p
				kept++
			} else {
				d.ids.delete(p)
			}
		}
	}
	// Free the blocks after the last one kept.
	rest := keep.next
	keep.n, keep.next = kept, nil
	for b := head; b != keep; b = b.next {
		b.n = uintptr(len(b.p))
	}
	for rest != nil {
		next := rest.next
		d.alloc.free(unsafe.Pointer(rest))
		rest = next
	}
	if keep == head && kept == 0 {
		d.alloc.free(unsafe.Pointer(head))
		d.spans.delete(s.base())
	}
	releasem(mp)
}

// slot returns the slot where p's entry would be if no other entry were
// in the way: Fibonacci hashing, which spreads objects of one size class,
// whose addresses are evenly spaced.
func (t *detPtrTable) slot(p uintptr) uintptr {
	return uintptr((uint64(p) * 0x9e3779b97f4a7c15) >> t.shift)
}

// find returns p's entry, or nil.
func (t *detPtrTable) find(p uintptr) *detPtrIDEntry {
	if t.n == 0 {
		return nil
	}
	mask := uintptr(len(t.e) - 1)
	for i := t.slot(p); ; i = (i + 1) & mask {
		e := &t.e[i]
		if e.p == p {
			return e
		}
		if e.p == 0 {
			return nil
		}
	}
}

// insert adds an entry for p, which has none.
func (t *detPtrTable) insert(p uintptr, v uint64) {
	if 2*(t.n+1) > len(t.e) {
		old := t.e
		n := max(2*len(old), 1<<10)
		mem := sysAlloc(uintptr(n)*unsafe.Sizeof(detPtrIDEntry{}), &memstats.other_sys, "detsched pointer identities")
		if mem == nil {
			throw("detsched: out of memory for pointer identities")
		}
		t.e = unsafe.Slice((*detPtrIDEntry)(mem), n)
		t.shift = uint(64 - sys.TrailingZeros64(uint64(n)))
		for _, e := range old {
			if e.p != 0 {
				t.put(e)
			}
		}
		if old != nil {
			sysFree(unsafe.Pointer(&old[0]), uintptr(len(old))*unsafe.Sizeof(detPtrIDEntry{}), &memstats.other_sys)
		}
	}
	t.put(detPtrIDEntry{p, v})
	t.n++
}

func (t *detPtrTable) put(e detPtrIDEntry) {
	mask := uintptr(len(t.e) - 1)
	i := t.slot(e.p)
	for t.e[i].p != 0 {
		i = (i + 1) & mask
	}
	t.e[i] = e
}

// delete removes p's entry, which it has, shifting back the entries
// after it that would no longer be found.
func (t *detPtrTable) delete(p uintptr) {
	mask := uintptr(len(t.e) - 1)
	i := t.slot(p)
	for t.e[i].p != p {
		i = (i + 1) & mask
	}
	for j := (i + 1) & mask; t.e[j].p != 0; j = (j + 1) & mask {
		// The entry at j may move to the hole at i if its home slot
		// is not in (i, j], cyclically.
		if h := t.slot(t.e[j].p); (j-h)&mask >= (j-i)&mask {
			t.e[i] = t.e[j]
			i = j
		}
	}
	t.e[i] = detPtrIDEntry{}
	t.n--
}
