package acorn

import (
	"bytes"
	"fmt"
)

// maxDepth guards against loops in a damaged file. A tree of 4 KiB pages
// with four cells per node at the very least is far below this.
const maxDepth = 64

// Tx is a transaction. A write transaction keeps its own copy of every
// node it changes; nothing reaches the file until Commit, and dropping
// the copies is a rollback.
type Tx struct {
	db       *DB
	meta     meta
	nodes    map[uint64]*node // changed in this transaction
	writable bool
	done     bool
	err      error // a write failed halfway; the tx must not commit
}

// read returns a node for reading: the transaction's own copy if it has
// one, otherwise the committed node.
func (tx *Tx) read(id uint64) (*node, error) {
	if n, ok := tx.nodes[id]; ok {
		return n, nil
	}
	return tx.db.pager.node(id)
}

// write returns a node the transaction may change.
func (tx *Tx) write(id uint64) (*node, error) {
	if n, ok := tx.nodes[id]; ok {
		return n, nil
	}
	n, err := tx.db.pager.node(id)
	if err != nil {
		return nil, err
	}
	c := n.clone()
	tx.nodes[id] = c
	return c, nil
}

// alloc returns a new empty node, reusing a free page when there is one.
func (tx *Tx) alloc(kind nodeKind) (*node, error) {
	id := tx.meta.freeHead
	if id != 0 {
		free, err := tx.read(id)
		if err != nil {
			return nil, err
		}
		if free.kind != kindFree {
			return nil, fmt.Errorf("%w: free list points to a used page %d", ErrCorrupt, id)
		}
		tx.meta.freeHead = free.link
	} else {
		id = tx.meta.pageCount
		tx.meta.pageCount++
	}
	n := &node{id: id, kind: kind}
	tx.nodes[id] = n
	return n, nil
}

// free puts a page at the head of the free list.
func (tx *Tx) free(id uint64) {
	tx.nodes[id] = &node{id: id, kind: kindFree, link: tx.meta.freeHead}
	tx.meta.freeHead = id
}

func (tx *Tx) check(write bool) error {
	if tx.done {
		return ErrTxDone
	}
	if write && !tx.writable {
		return ErrReadOnly
	}
	return nil
}

// Get returns a copy of the value stored under key, or ErrNotFound.
func (tx *Tx) Get(key []byte) ([]byte, error) {
	if err := tx.check(false); err != nil {
		return nil, err
	}
	n, err := tx.findLeaf(key)
	if err != nil {
		return nil, err
	}
	i, found := n.search(key)
	if !found {
		return nil, ErrNotFound
	}
	return bytes.Clone(n.vals[i]), nil
}

// findLeaf walks from the root to the leaf that may hold key.
func (tx *Tx) findLeaf(key []byte) (*node, error) {
	id := tx.meta.root
	for range maxDepth {
		n, err := tx.read(id)
		if err != nil {
			return nil, err
		}
		switch n.kind {
		case kindLeaf:
			return n, nil
		case kindInternal:
			id = n.children[n.childIndex(key)]
		default:
			return nil, fmt.Errorf("%w: free page %d inside the tree", ErrCorrupt, id)
		}
	}
	return nil, fmt.Errorf("%w: tree deeper than %d", ErrCorrupt, maxDepth)
}

// Put stores value under key, replacing any old value.
func (tx *Tx) Put(key, value []byte) error {
	if err := tx.check(true); err != nil {
		return err
	}
	if len(key) == 0 || len(key) > MaxKeySize {
		return fmt.Errorf("%w: key must be 1 to %d bytes", ErrTooLarge, MaxKeySize)
	}
	if len(key)+len(value) > MaxEntrySize {
		return fmt.Errorf("%w: key and value over %d bytes", ErrTooLarge, MaxEntrySize)
	}
	// The caller may reuse its buffers, so keep our own copies.
	err := tx.insert(tx.meta.root, bytes.Clone(key), bytes.Clone(value), 0)
	if err == nil {
		err = tx.fixRoot()
	}
	return tx.fail(err)
}

// fail remembers an error from a write that may have stopped halfway,
// so Update will not commit a half-changed tree even if fn ignores it.
func (tx *Tx) fail(err error) error {
	if err != nil && tx.err == nil {
		tx.err = err
	}
	return err
}

// Delete removes key. Deleting a missing key is not an error.
func (tx *Tx) Delete(key []byte) error {
	if err := tx.check(true); err != nil {
		return err
	}
	found, err := tx.delete(tx.meta.root, key, 0)
	if err == nil && found {
		err = tx.fixRoot()
	}
	return tx.fail(err)
}

// Scan calls fn for every key in [start, end) in order, until fn returns
// false. A nil start means from the first key, a nil end means to the
// last. The slices passed to fn must not be changed or kept.
func (tx *Tx) Scan(start, end []byte, fn func(key, value []byte) bool) error {
	if err := tx.check(false); err != nil {
		return err
	}
	n, err := tx.findLeaf(start)
	if err != nil {
		return err
	}
	i, _ := n.search(start)
	// Leaves are linked left to right, so a scan never goes back up the
	// tree. The step limit stops a damaged link from looping forever.
	for steps := uint64(0); steps < tx.meta.pageCount; steps++ {
		for ; i < len(n.keys); i++ {
			if end != nil && bytes.Compare(n.keys[i], end) >= 0 {
				return nil
			}
			if !fn(n.keys[i], n.vals[i]) {
				return nil
			}
		}
		if n.link == 0 {
			return nil
		}
		if n, err = tx.read(n.link); err != nil {
			return err
		}
		if !n.leaf() {
			return fmt.Errorf("%w: leaf link to page %d", ErrCorrupt, n.id)
		}
		i = 0
	}
	return fmt.Errorf("%w: loop in the leaf links", ErrCorrupt)
}
