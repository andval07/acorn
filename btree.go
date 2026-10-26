package acorn

import (
	"fmt"
	"slices"
)

// Inserts and deletes change a leaf first, then fix the tree on the way
// back up: after the call into child i returns, the parent looks at the
// child's size and splits it (too big) or rebalances it with a sibling
// (too small). The root has no parent, so fixRoot handles it last.

func (tx *Tx) insert(id uint64, key, value []byte, depth int) error {
	if depth >= maxDepth {
		return fmt.Errorf("%w: tree deeper than %d", ErrCorrupt, maxDepth)
	}
	n, err := tx.write(id)
	if err != nil {
		return err
	}
	if n.leaf() {
		i, found := n.search(key)
		if found {
			n.vals[i] = value
		} else {
			n.keys = slices.Insert(n.keys, i, key)
			n.vals = slices.Insert(n.vals, i, value)
		}
		return nil
	}
	if n.kind != kindInternal {
		return fmt.Errorf("%w: free page %d inside the tree", ErrCorrupt, id)
	}
	ci := n.childIndex(key)
	if err := tx.insert(n.children[ci], key, value, depth+1); err != nil {
		return err
	}
	return tx.fixChild(n, ci)
}

// delete reports whether key was there. Pages are only copied for writing
// once the key is found, so deleting a missing key changes nothing.
func (tx *Tx) delete(id uint64, key []byte, depth int) (bool, error) {
	if depth >= maxDepth {
		return false, fmt.Errorf("%w: tree deeper than %d", ErrCorrupt, maxDepth)
	}
	n, err := tx.read(id)
	if err != nil {
		return false, err
	}
	if n.leaf() {
		i, found := n.search(key)
		if !found {
			return false, nil
		}
		if n, err = tx.write(id); err != nil {
			return false, err
		}
		n.keys = slices.Delete(n.keys, i, i+1)
		n.vals = slices.Delete(n.vals, i, i+1)
		return true, nil
	}
	if n.kind != kindInternal {
		return false, fmt.Errorf("%w: free page %d inside the tree", ErrCorrupt, id)
	}
	ci := n.childIndex(key)
	found, err := tx.delete(n.children[ci], key, depth+1)
	if err != nil || !found {
		return found, err
	}
	if n, err = tx.write(id); err != nil {
		return false, err
	}
	return true, tx.fixChild(n, ci)
}

// fixChild splits or rebalances child ci of parent after it changed.
func (tx *Tx) fixChild(parent *node, ci int) error {
	child, err := tx.read(parent.children[ci])
	if err != nil {
		return err
	}
	switch {
	case child.size() > PageSize:
		return tx.split(parent, ci)
	case child.size() < minFill && len(parent.children) > 1:
		return tx.rebalance(parent, ci)
	}
	return nil
}

// split moves the right half of child ci into a new node and adds the
// separator key to the parent. The parent may now be too big itself;
// its own parent fixes that on the next step up.
func (tx *Tx) split(parent *node, ci int) error {
	left, err := tx.write(parent.children[ci])
	if err != nil {
		return err
	}
	right, err := tx.alloc(left.kind)
	if err != nil {
		return err
	}
	sep := splitInto(left, right)
	if left.size() > PageSize || right.size() > PageSize {
		return fmt.Errorf("acorn: split of page %d left a node too big", left.id)
	}
	parent.keys = slices.Insert(parent.keys, ci, sep)
	parent.children = slices.Insert(parent.children, ci+1, right.id)
	return nil
}

// splitInto moves the upper half of left into right (which must be empty)
// and returns the key that separates them in the parent.
func splitInto(left, right *node) []byte {
	m := left.splitPoint()
	if left.leaf() {
		right.keys = slices.Clone(left.keys[m:])
		right.vals = slices.Clone(left.vals[m:])
		left.keys = slices.Clip(left.keys[:m])
		left.vals = slices.Clip(left.vals[:m])
		// Keep the leaf chain in order: left -> right -> old next.
		right.link = left.link
		left.link = right.id
		return right.keys[0]
	}
	// In an internal node the middle key moves up instead of being copied.
	sep := left.keys[m]
	right.keys = slices.Clone(left.keys[m+1:])
	right.children = slices.Clone(left.children[m+1:])
	left.keys = slices.Clip(left.keys[:m])
	left.children = slices.Clip(left.children[:m+1])
	return sep
}

// rebalance joins child ci with a sibling. If everything fits in one page
// the two nodes merge and one page is freed; otherwise the cells are
// split again evenly between them. Both cases start from the same list
// of cells, which keeps the code short.
func (tx *Tx) rebalance(parent *node, ci int) error {
	li := ci - 1 // the left node of the pair; parent.keys[li] separates them
	if ci == 0 {
		li = 0
	}
	left, err := tx.write(parent.children[li])
	if err != nil {
		return err
	}
	right, err := tx.write(parent.children[li+1])
	if err != nil {
		return err
	}

	// Gather all cells into left. For internal nodes the separator comes
	// down from the parent, since it sits between the two halves.
	if left.leaf() {
		left.keys = append(left.keys, right.keys...)
		left.vals = append(left.vals, right.vals...)
		left.link = right.link
	} else {
		left.keys = append(append(left.keys, parent.keys[li]), right.keys...)
		left.children = append(left.children, right.children...)
	}

	if left.size() <= PageSize {
		parent.keys = slices.Delete(parent.keys, li, li+1)
		parent.children = slices.Delete(parent.children, li+1, li+2)
		tx.free(right.id)
		return nil
	}

	*right = node{id: right.id, kind: right.kind}
	parent.keys[li] = splitInto(left, right)
	return nil
}

// fixRoot runs after every change. A root that is too big is split under
// a new root, so the tree grows at the top and all leaves stay at the
// same depth. An internal root left with one child is removed, which is
// how the tree gets shorter.
func (tx *Tx) fixRoot() error {
	root, err := tx.read(tx.meta.root)
	if err != nil {
		return err
	}
	if root.size() > PageSize {
		newRoot, err := tx.alloc(kindInternal)
		if err != nil {
			return err
		}
		newRoot.children = []uint64{root.id}
		tx.meta.root = newRoot.id
		return tx.split(newRoot, 0)
	}
	if root.kind == kindInternal && len(root.keys) == 0 {
		tx.meta.root = root.children[0]
		tx.free(root.id)
	}
	return nil
}
