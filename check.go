package acorn

import (
	"bytes"
	"fmt"
)

// Stats describes the shape of the tree.
type Stats struct {
	Depth     int    // levels, 1 for a tree that is a single leaf
	Pages     uint64 // pages in the file, meta page included
	FreePages uint64
	Keys      int
}

// Check walks the whole file and returns an error if any invariant is
// broken: keys out of order or outside their parent's bounds, leaves at
// different depths, a node over a page, a broken leaf chain, or a page
// that is neither in the tree nor on the free list (a leak).
func (db *DB) Check() error {
	return db.View(func(tx *Tx) error {
		_, err := tx.verify()
		return err
	})
}

// Stats returns the tree shape. It walks the whole tree, like Check.
func (db *DB) Stats() (Stats, error) {
	var s Stats
	err := db.View(func(tx *Tx) error {
		var err error
		s, err = tx.verify()
		return err
	})
	return s, err
}

type verifier struct {
	tx     *Tx
	seen   []bool
	leaves []uint64
	depth  int // depth of the first leaf found, 0 before that
	keys   int
}

func (tx *Tx) verify() (Stats, error) {
	v := &verifier{tx: tx, seen: make([]bool, tx.meta.pageCount)}
	v.seen[0] = true
	if err := v.walk(tx.meta.root, nil, nil, 1); err != nil {
		return Stats{}, err
	}
	if err := v.checkChain(); err != nil {
		return Stats{}, err
	}
	free, err := v.checkFreeList()
	if err != nil {
		return Stats{}, err
	}
	for id, ok := range v.seen {
		if !ok {
			return Stats{}, fmt.Errorf("page %d is lost: not in the tree or the free list", id)
		}
	}
	return Stats{Depth: v.depth, Pages: tx.meta.pageCount, FreePages: free, Keys: v.keys}, nil
}

// mark records a visit, so a page reached twice is caught.
func (v *verifier) mark(id uint64) error {
	if id == 0 || id >= uint64(len(v.seen)) {
		return fmt.Errorf("page %d out of range", id)
	}
	if v.seen[id] {
		return fmt.Errorf("page %d reached twice", id)
	}
	v.seen[id] = true
	return nil
}

// walk checks the subtree at id. Every key in it must be in [lo, hi),
// where nil means no bound.
func (v *verifier) walk(id uint64, lo, hi []byte, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("tree deeper than %d", maxDepth)
	}
	if err := v.mark(id); err != nil {
		return err
	}
	n, err := v.tx.read(id)
	if err != nil {
		return err
	}
	if n.size() > PageSize {
		return fmt.Errorf("page %d is %d bytes", id, n.size())
	}
	if id != v.tx.meta.root && n.size() < minFill {
		return fmt.Errorf("page %d is under-full: %d bytes", id, n.size())
	}
	for i, k := range n.keys {
		if i > 0 && bytes.Compare(n.keys[i-1], k) >= 0 {
			return fmt.Errorf("page %d: keys out of order at %d", id, i)
		}
		if (lo != nil && bytes.Compare(k, lo) < 0) || (hi != nil && bytes.Compare(k, hi) >= 0) {
			return fmt.Errorf("page %d: key %q outside its parent's range", id, k)
		}
	}

	switch n.kind {
	case kindLeaf:
		if v.depth == 0 {
			v.depth = depth
		} else if depth != v.depth {
			return fmt.Errorf("leaf %d at depth %d, others at %d", id, depth, v.depth)
		}
		v.leaves = append(v.leaves, id)
		v.keys += len(n.keys)
		return nil
	case kindInternal:
		if len(n.children) != len(n.keys)+1 {
			return fmt.Errorf("page %d: %d keys but %d children", id, len(n.keys), len(n.children))
		}
		for i, child := range n.children {
			clo, chi := lo, hi
			if i > 0 {
				clo = n.keys[i-1]
			}
			if i < len(n.keys) {
				chi = n.keys[i]
			}
			if err := v.walk(child, clo, chi, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("free page %d inside the tree", id)
}

// checkChain makes sure the leaf links visit the leaves in tree order.
func (v *verifier) checkChain() error {
	for i, id := range v.leaves {
		n, err := v.tx.read(id)
		if err != nil {
			return err
		}
		var want uint64
		if i+1 < len(v.leaves) {
			want = v.leaves[i+1]
		}
		if n.link != want {
			return fmt.Errorf("leaf %d links to %d, want %d", id, n.link, want)
		}
	}
	return nil
}

func (v *verifier) checkFreeList() (uint64, error) {
	var count uint64
	for id := v.tx.meta.freeHead; id != 0; count++ {
		if err := v.mark(id); err != nil {
			return 0, fmt.Errorf("free list: %w", err)
		}
		n, err := v.tx.read(id)
		if err != nil {
			return 0, err
		}
		if n.kind != kindFree {
			return 0, fmt.Errorf("free list holds used page %d", id)
		}
		id = n.link
	}
	return count, nil
}
