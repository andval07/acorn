// Package acorn is a small embedded key-value store: a B+tree of 4 KiB
// pages in one file, with a write-ahead log for crash safety.
package acorn

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrCorrupt  = errors.New("acorn: file is corrupt")
	ErrNotFound = errors.New("acorn: key not found")
	ErrReadOnly = errors.New("acorn: write in a read-only transaction")
	ErrTxDone   = errors.New("acorn: transaction already finished")
	ErrTooLarge = errors.New("acorn: entry too large")
	ErrClosed   = errors.New("acorn: database is closed")
)

// Options tune a database. The zero value is a safe default.
type Options struct {
	// NoSync skips fsync on commit. Faster, but a power cut can lose the
	// last commits (the file still opens in a consistent state).
	NoSync bool
	// CacheNodes is how many decoded pages to keep in memory.
	CacheNodes int
	// CheckpointPages is how many changed pages the WAL may hold before
	// they are copied into the main file.
	CheckpointPages int
}

const (
	defaultCacheNodes      = 4096
	defaultCheckpointPages = 2048
)

// DB is an open database. It is safe for use from many goroutines: reads
// run in parallel, writes one at a time.
type DB struct {
	mu     sync.RWMutex
	pager  *pager
	err    error // set when a commit or checkpoint failed; blocks writes
	closed bool
}

// Open opens the database at path, creating it if needed. If the last
// run crashed, committed transactions are recovered from the WAL.
func Open(path string, opts Options) (*DB, error) {
	if opts.CacheNodes <= 0 {
		opts.CacheNodes = defaultCacheNodes
	}
	if opts.CheckpointPages <= 0 {
		opts.CheckpointPages = defaultCheckpointPages
	}
	p, err := openPager(path, opts)
	if err != nil {
		return nil, err
	}
	return &DB{pager: p}, nil
}

// Update runs fn in a write transaction. If fn returns nil the changes
// are committed, otherwise they are dropped.
func (db *DB) Update(fn func(tx *Tx) error) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if db.err != nil {
		return db.err
	}
	tx := &Tx{db: db, meta: db.pager.meta, nodes: map[uint64]*node{}, writable: true}
	err := fn(tx)
	tx.done = true
	if err != nil {
		return err
	}
	if tx.err != nil {
		return fmt.Errorf("acorn: transaction not committed: %w", tx.err)
	}
	if len(tx.nodes) == 0 && tx.meta == db.pager.meta {
		return nil
	}
	// After a failed write or fsync we no longer know what is on disk.
	// Refusing more writes until the file is reopened (and the WAL
	// replayed) is the only safe answer.
	if err := db.pager.commit(tx.nodes, tx.meta); err != nil {
		db.err = fmt.Errorf("acorn: commit failed, reopen the database: %w", err)
		return db.err
	}
	if db.pager.needsCheckpoint() {
		if err := db.pager.checkpoint(); err != nil {
			// The commit itself is safe in the WAL, so report success.
			db.err = fmt.Errorf("acorn: checkpoint failed, reopen the database: %w", err)
		}
	}
	return nil
}

// View runs fn in a read-only transaction.
func (db *DB) View(fn func(tx *Tx) error) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return ErrClosed
	}
	tx := &Tx{db: db, meta: db.pager.meta}
	defer func() { tx.done = true }()
	return fn(tx)
}

// Get returns the value stored under key, or ErrNotFound.
func (db *DB) Get(key []byte) ([]byte, error) {
	var v []byte
	err := db.View(func(tx *Tx) error {
		var err error
		v, err = tx.Get(key)
		return err
	})
	return v, err
}

// Put stores value under key in its own transaction.
func (db *DB) Put(key, value []byte) error {
	return db.Update(func(tx *Tx) error { return tx.Put(key, value) })
}

// Delete removes key in its own transaction.
func (db *DB) Delete(key []byte) error {
	return db.Update(func(tx *Tx) error { return tx.Delete(key) })
}

// Scan calls fn for every key in [start, end); see Tx.Scan.
func (db *DB) Scan(start, end []byte, fn func(key, value []byte) bool) error {
	return db.View(func(tx *Tx) error { return tx.Scan(start, end, fn) })
}

// Close copies the WAL into the main file and closes both files.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	db.closed = true
	var err error
	if db.err == nil {
		err = db.pager.checkpoint()
	}
	if cerr := db.pager.close(); err == nil {
		err = cerr
	}
	return err
}

// abandon closes the files without a checkpoint, leaving the WAL as a
// crash would. Tests use it.
func (db *DB) abandon() {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.closed = true
	db.pager.close()
}
