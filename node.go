package acorn

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

// Page layout (all integers little endian):
//
//	0      kind (1 byte)
//	2..4   number of cells
//	8..16  link: next leaf (leaf), first child (internal), next free page (free)
//	16..   one uint16 offset per cell, then the cells
//
// Leaf cell:     klen u16 | vlen u16 | key | value
// Internal cell: klen u16 | child u64 | key
//
// An internal node with keys k0..kn-1 has children c0..cn: c0 is in the
// header and ci+1 is stored next to ki. Child ci+1 holds the keys >= ki.
const (
	PageSize   = 4096
	headerSize = 16

	// MaxKeySize and MaxEntrySize keep every cell under a quarter of a page,
	// so a node always holds at least four cells and a split always works.
	MaxKeySize   = 512
	MaxEntrySize = 1000

	// A node that uses less than this is merged with or refilled from a
	// sibling after a delete.
	minFill = PageSize / 4
)

type nodeKind byte

const (
	kindLeaf     nodeKind = 1
	kindInternal nodeKind = 2
	kindFree     nodeKind = 3
)

// node is a decoded page. Nodes in the cache are shared and never changed:
// a write transaction clones a node before modifying it. Key and value
// slices are never modified in place either, so a clone only copies the
// outer slices.
type node struct {
	id       uint64
	kind     nodeKind
	keys     [][]byte
	vals     [][]byte // leaf only
	children []uint64 // internal only, len(keys)+1
	link     uint64   // next leaf, or next free page
}

func (n *node) leaf() bool { return n.kind == kindLeaf }

func (n *node) clone() *node {
	c := *n
	c.keys = append([][]byte(nil), n.keys...)
	c.vals = append([][]byte(nil), n.vals...)
	c.children = append([]uint64(nil), n.children...)
	return &c
}

func leafCellSize(k, v []byte) int { return 2 + 4 + len(k) + len(v) }
func innerCellSize(k []byte) int   { return 2 + 2 + 8 + len(k) }

// size is the number of bytes the node needs once encoded. It can be more
// than PageSize for a moment, until the parent splits the node.
func (n *node) size() int {
	s := headerSize
	for i, k := range n.keys {
		if n.leaf() {
			s += leafCellSize(k, n.vals[i])
		} else {
			s += innerCellSize(k)
		}
	}
	return s
}

// search returns the position of key in a leaf, or where it would go.
func (n *node) search(key []byte) (int, bool) {
	i := sort.Search(len(n.keys), func(i int) bool {
		return bytes.Compare(n.keys[i], key) >= 0
	})
	return i, i < len(n.keys) && bytes.Equal(n.keys[i], key)
}

// childIndex returns which child of an internal node may hold key.
func (n *node) childIndex(key []byte) int {
	return sort.Search(len(n.keys), func(i int) bool {
		return bytes.Compare(key, n.keys[i]) < 0
	})
}

func (n *node) encode(buf []byte) {
	clear(buf)
	buf[0] = byte(n.kind)
	binary.LittleEndian.PutUint16(buf[2:], uint16(len(n.keys)))
	link := n.link
	if n.kind == kindInternal {
		link = n.children[0]
	}
	binary.LittleEndian.PutUint64(buf[8:], link)

	off := headerSize + 2*len(n.keys)
	for i, k := range n.keys {
		binary.LittleEndian.PutUint16(buf[headerSize+2*i:], uint16(off))
		binary.LittleEndian.PutUint16(buf[off:], uint16(len(k)))
		if n.leaf() {
			v := n.vals[i]
			binary.LittleEndian.PutUint16(buf[off+2:], uint16(len(v)))
			off += 4
			off += copy(buf[off:], k)
			off += copy(buf[off:], v)
		} else {
			binary.LittleEndian.PutUint64(buf[off+2:], n.children[i+1])
			off += 10
			off += copy(buf[off:], k)
		}
	}
}

// decode reads a page. It checks every length against the page bounds,
// so a damaged file gives an error instead of a panic. Keys and values
// are copied, so the node does not keep the page buffer alive.
func decode(id uint64, buf []byte) (*node, error) {
	bad := func(what string) (*node, error) {
		return nil, fmt.Errorf("%w: page %d: %s", ErrCorrupt, id, what)
	}
	n := &node{id: id, kind: nodeKind(buf[0])}
	count := int(binary.LittleEndian.Uint16(buf[2:]))
	link := binary.LittleEndian.Uint64(buf[8:])

	switch n.kind {
	case kindFree:
		n.link = link
		return n, nil
	case kindLeaf:
		n.link = link
		n.keys = make([][]byte, count)
		n.vals = make([][]byte, count)
	case kindInternal:
		n.keys = make([][]byte, count)
		n.children = make([]uint64, count+1)
		n.children[0] = link
	default:
		return bad(fmt.Sprintf("unknown kind %d", buf[0]))
	}
	if headerSize+2*count > len(buf) {
		return bad("too many cells")
	}

	for i := range count {
		off := int(binary.LittleEndian.Uint16(buf[headerSize+2*i:]))
		cellHeader := 4 // klen, vlen
		if !n.leaf() {
			cellHeader = 10 // klen, child
		}
		if off+cellHeader > len(buf) {
			return bad("cell offset out of range")
		}
		klen := int(binary.LittleEndian.Uint16(buf[off:]))
		if n.leaf() {
			vlen := int(binary.LittleEndian.Uint16(buf[off+2:]))
			start := off + 4
			if start+klen+vlen > len(buf) {
				return bad("cell out of range")
			}
			n.keys[i] = bytes.Clone(buf[start : start+klen])
			n.vals[i] = bytes.Clone(buf[start+klen : start+klen+vlen])
		} else {
			n.children[i+1] = binary.LittleEndian.Uint64(buf[off+2:])
			start := off + 10
			if start+klen > len(buf) {
				return bad("cell out of range")
			}
			n.keys[i] = bytes.Clone(buf[start : start+klen])
		}
	}
	return n, nil
}

// splitPoint picks how many cells go to the left half so both halves use
// about the same number of bytes. For an internal node the cell at the
// returned index moves up to the parent instead.
func (n *node) splitPoint() int {
	// Keep at least one key on each side. An internal node also gives
	// one key to the parent, so its right half needs one more.
	last := len(n.keys) - 1
	if !n.leaf() {
		last--
	}
	total := n.size()
	used := headerSize
	for i, k := range n.keys {
		if n.leaf() {
			used += leafCellSize(k, n.vals[i])
		} else {
			used += innerCellSize(k)
		}
		if used >= total/2 {
			return max(1, min(i, last))
		}
	}
	return max(1, min(len(n.keys)/2, last))
}

// appendSplitPoint is splitPoint for keys that arrive in order: the right
// half gets just enough cells to reach minFill, and the left keeps the
// rest. It falls back to splitPoint if the left half would not fit.
func (n *node) appendSplitPoint() int {
	used := headerSize
	for m := len(n.keys) - 1; m >= 1; m-- {
		// In an internal node key m moves up, so the right half starts
		// at m+1; its leftmost child needs no key of its own.
		if n.leaf() {
			used += leafCellSize(n.keys[m], n.vals[m])
		} else if m+1 < len(n.keys) {
			used += innerCellSize(n.keys[m+1])
		}
		if used < minFill {
			continue
		}
		left := n.size() - used + headerSize
		if !n.leaf() {
			left -= innerCellSize(n.keys[m]) // the separator moves up
		}
		if left <= PageSize {
			return m
		}
		break
	}
	return n.splitPoint()
}
