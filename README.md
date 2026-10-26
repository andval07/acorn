# acorn

An embedded key-value store in Go, built from scratch: a **B+tree** on 4 KB
pages, a **write-ahead log** for crash safety, and a free list so deleted pages
are reused. Standard library only. About 1,300 lines of engine code and 830
lines of tests.

```go
db, err := acorn.Open("data.acorn", acorn.Options{})
if err != nil {
	log.Fatal(err)
}
defer db.Close()

err = db.Update(func(tx *acorn.Tx) error {
	if err := tx.Put([]byte("alice"), []byte("42")); err != nil {
		return err
	}
	return tx.Delete([]byte("bob"))
}) // both changes are on disk, or neither is

v, err := db.Get([]byte("alice"))
err = db.Scan([]byte("a"), []byte("c"), func(k, v []byte) bool {
	fmt.Printf("%s = %s\n", k, v)
	return true // false stops the scan
})
```

There is also a small CLI:

```
go build ./cmd/acorn
acorn data.acorn put alice 42
acorn data.acorn get alice
acorn data.acorn scan a c
acorn data.acorn stats     # keys, depth, pages, free pages
acorn data.acorn check     # full structural check of the file
```

## Features

- Ordered keys (byte order), point reads, range scans over `[start, end)`.
- Transactions: `Update` is atomic and durable (fsync on commit), `View` sees
  a stable snapshot. Returning an error from the function rolls back.
- Keys up to 512 bytes, key + value up to 1000 bytes.
- Crash recovery from the WAL on open. Tested by cutting the log at hundreds
  of points, flipping bytes, stopping checkpoints halfway and killing the
  process (see [Tests](#tests)).
- Deleted pages go to a free list and are reused before the file grows.
- `Check()` walks the whole file and verifies every invariant of the tree.

## Design

```
 DB  ── Update / View ──►  Tx  ── Put / Delete / Get / Scan ──►  B+tree
                                                                   │
                                     changed nodes (copy-on-write) │
                                                                   ▼
             main file  ◄── checkpoint ──  pending pages  ◄──  WAL (commit)
```

**Pages.** The file is an array of 4096-byte pages. Page 0 is the meta record:
magic, root page, page count, head of the free list, and a CRC. Every other
page is a leaf, an internal node or a free page:

```
0       kind
2..4    number of cells
8..16   link: next leaf / first child / next free page
16..    one uint16 offset per cell, then the cells

leaf cell:     klen u16 | vlen u16 | key | value
internal cell: klen u16 | child u64 | key
```

All values live in the leaves, and the leaves are chained left to right, so a
range scan finds the first leaf once and then follows the links.

**Copy-on-write.** Decoded pages are cached and never modified in place. A
write transaction clones a node the first time it changes it and keeps its own
copies. Rolling back means dropping that map; nothing shared was touched.

**Insert and delete.** Both are recursive. On the way back up, each parent
fixes the child it went into:

- a child bigger than a page is split in two, and the separator key moves up;
- a child under a quarter of a page is merged with a sibling if the two fit in
  one page, otherwise the two are split again evenly;
- a root that overflows gets a new root above it; an internal root left with
  one child is removed, so the tree gets one level shorter.

The size limits keep every cell under a quarter of a page, so a node always
holds at least four cells and a split always produces two valid nodes.

**WAL.** A commit appends one frame per changed page, then the meta page with
a commit flag, in a single write followed by fsync. Each frame is a full page
image with a CRC. The commit point is the meta frame: on open, the log is
replayed up to the last complete commit, and anything after it (a torn write)
is ignored. Because the meta page holds the root and the free list, a commit
switches the whole tree state at once.

**Checkpoint.** Committed pages stay in memory until the WAL holds 2048 of
them (configurable). Then they are written into the main file, the main file
is synced, and only after that the WAL is truncated. A crash in the middle is
safe: the WAL still has every page, and writing the same page image twice
gives the same page.

**Failure handling.** If a commit or a checkpoint fails, the DB refuses
further writes until it is reopened (reads still work), because the state in
memory may no longer match the disk. A checkpoint failure does not fail the
commit that triggered it: that commit is already safe in the WAL.

## Tests

```
go test ./...              # about 15 s, coverage 84%
go test -short ./...       # skips the process-kill test
go test -bench . -run ^$   # micro-benchmarks
```

- **Model test.** About 24,000 random puts and deletes in 1,200 transactions,
  mirrored in a Go map. A full `Check()` after every transaction, a reopen
  every 50, and at the end every key is compared with the map through `Get`
  and `Scan`. Four seeds, with phases that grow and shrink the tree so splits
  and merges both run. Rollback has its own test.
- **Torn WAL.** Twelve commits are left in the log, then the log is cut at 262
  different lengths. After each cut the DB must open, pass `Check()`, and hold
  exactly the state of the last commit fully inside the cut. Never a mix.
- **Corrupt frame.** One flipped byte in the last commit drops that commit
  only.
- **Crash during checkpoint.** A test hook stops the checkpoint before the
  first, second, middle and last page. The next open must recover the full
  state.
- **Process kill.** A child process commits one key per transaction with fsync
  and prints each acknowledged commit. The parent kills it at a random point
  (three times), reopens the file and checks that every acknowledged commit
  is there and that the keys have no gaps.
- **Page reuse.** Delete everything, refill, and the file does not grow.
- Node encoding round trips, and decoding of damaged pages returns
  `ErrCorrupt` instead of panicking.

## Benchmark against SQLite

`bench/` is a separate module that runs the same workloads on acorn and on
SQLite: 100,000 keys of 16 bytes, values of 100 bytes, fsync on commit for
both. SQLite uses a `WITHOUT ROWID` table with the key as primary key,
`journal_mode=WAL`, `synchronous=FULL`, a 16 MB cache, and prepared
statements.

```
cd bench && go run . -runs 5
```

Measured on an Intel i9-13950HX laptop with an NVMe SSD, Windows 11, Go 1.27.
Each run reports the median of several repetitions; the table shows the lowest
and highest median over three runs, because the numbers move between runs.

| workload | acorn | SQLite |
|---|---:|---:|
| insert 100k sequential keys, 1 tx | 744k – 823k keys/s | 435k – 471k keys/s |
| insert 100k random keys, 1 tx | 790k – 895k keys/s | 174k – 444k keys/s |
| get, random keys | 461k – 1.12M ops/s | 79k – 80k ops/s |
| full range scan | 4.87M – 6.27M keys/s | 1.19M – 1.48M keys/s |
| 1000 commits of 1 key, fsync each | 1.4k – 1.5k commits/s | 1.4k – 1.5k commits/s |

How to read this honestly:

- The SQLite driver is [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite),
  a translation of SQLite's C code to Go, chosen because this machine has no C
  compiler for cgo. It is slower than the original C library, so this compares
  acorn with that driver, not with SQLite at its best.
- SQLite does much more work per call: it goes through `database/sql`, parses
  and runs SQL through its virtual machine, and handles types. acorn is a
  direct function call into the tree. Most of the gap on reads comes from
  that, not from a better B-tree.
- Small synced commits are the same for both, about 0.7 ms each. That is the
  cost of fsync on this disk, and neither engine can go faster than the disk.

## Limitations

- **One writer at a time.** `Update` takes an exclusive lock, so it also
  blocks readers while it runs. `View` calls run in parallel with each other.
  There is no MVCC.
- **No overflow pages.** Values over about 1 KB are rejected instead of being
  split across pages.
- **Half-full leaves on sequential insert.** A split always cuts a node in the
  middle. When keys arrive in order, the left half is never touched again, so
  leaves stay about half full: 100k sequential keys use 6,336 pages against
  4,375 for the same keys in random order. SQLite avoids this with a split
  that puts almost everything on the left when the insert is at the right
  edge. That would be the next improvement.
- **Crash testing has limits.** The kill test proves that a process crash
  loses no acknowledged commit. A power cut also drops the operating system's
  write cache. Only fsync protects against that, and a test cannot reproduce
  it without special hardware or a fault-injecting file system.
- **No race detector run.** `go test -race` needs cgo, and this machine has no
  C compiler. Concurrency is kept simple instead: one `sync.RWMutex` around
  the whole DB, and shared pages that are never modified.
- Keys are compared as raw bytes. There are no secondary indexes, no
  compaction of a file that has shrunk, and no compression.

## Layout

```
db.go        Open, Update/View, options, errors, poisoning on failure
tx.go        transactions: Get, Put, Delete, Scan, page allocation
btree.go     insert, delete, split, merge/rebalance, root changes
node.go      page encoding and bounds-checked decoding
pager.go     page cache, pending pages, commit, checkpoint, recovery
wal.go       WAL frames, CRC, replay
meta.go      the meta page
cache.go     LRU cache of decoded pages
check.go     Check() and Stats(): full structural verification
cmd/acorn    CLI
bench/       benchmark against SQLite (separate module)
```
