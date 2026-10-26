package acorn

import (
	"fmt"
	"os"
	"slices"
)

// pager reads pages from the main file and commits transactions to the
// WAL. Committed pages stay in memory (pending) until a checkpoint copies
// them into the main file, so a read checks pending before the file.
type pager struct {
	file    *os.File
	wal     *wal
	meta    meta              // state of the last committed transaction
	pending map[uint64][]byte // in the WAL, not yet in the main file
	cache   *cache

	sync            bool
	checkpointPages int
}

// testCheckpointHook lets tests stop a checkpoint halfway, as a crash
// would. It is called before each page is copied.
var testCheckpointHook func(copied int) error

func openPager(path string, opts Options) (*pager, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	walFile, err := os.OpenFile(path+"-wal", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("open wal: %w", err)
	}
	p := &pager{
		file:            file,
		wal:             &wal{f: walFile},
		cache:           newCache(opts.CacheNodes),
		sync:            !opts.NoSync,
		checkpointPages: opts.CheckpointPages,
	}
	if err := p.load(); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

// load sets up a new file, or reads the meta page and replays the WAL.
func (p *pager) load() error {
	info, err := p.file.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if info.Size() == 0 {
		return p.create()
	}

	p.pending, err = p.wal.recover()
	if err != nil {
		return err
	}
	buf, ok := p.pending[0]
	if !ok {
		buf = make([]byte, PageSize)
		if _, err := p.file.ReadAt(buf, 0); err != nil {
			return fmt.Errorf("%w: read meta: %v", ErrCorrupt, err)
		}
	}
	if p.meta, err = decodeMeta(buf); err != nil {
		return err
	}
	// Move what the WAL holds into the main file now, so the log
	// starts empty.
	return p.checkpoint()
}

// create writes the meta page and an empty root leaf.
func (p *pager) create() error {
	p.meta = meta{root: 1, pageCount: 2}
	buf := make([]byte, 2*PageSize)
	p.meta.encode(buf[:PageSize])
	(&node{id: 1, kind: kindLeaf}).encode(buf[PageSize:])
	if _, err := p.file.WriteAt(buf, 0); err != nil {
		return fmt.Errorf("init file: %w", err)
	}
	if err := p.file.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	p.pending = map[uint64][]byte{}
	return nil
}

// node returns a committed page. The result is shared: never modify it.
func (p *pager) node(id uint64) (*node, error) {
	if id == 0 || id >= p.meta.pageCount {
		return nil, fmt.Errorf("%w: page %d out of range", ErrCorrupt, id)
	}
	if n, ok := p.cache.get(id); ok {
		return n, nil
	}
	buf, ok := p.pending[id]
	if !ok {
		buf = make([]byte, PageSize)
		if _, err := p.file.ReadAt(buf, int64(id)*PageSize); err != nil {
			return nil, fmt.Errorf("%w: read page %d: %v", ErrCorrupt, id, err)
		}
	}
	n, err := decode(id, buf)
	if err != nil {
		return nil, err
	}
	p.cache.put(n)
	return n, nil
}

// commit writes the changed nodes and then the meta page to the WAL.
// Only after that succeeds do the new pages become visible to readers.
func (p *pager) commit(nodes map[uint64]*node, m meta) error {
	ids := make([]uint64, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	frames := make([]frame, 0, len(ids)+1)
	for _, id := range ids {
		buf := make([]byte, PageSize)
		nodes[id].encode(buf)
		frames = append(frames, frame{id, buf})
	}
	metaBuf := make([]byte, PageSize)
	m.encode(metaBuf)
	frames = append(frames, frame{0, metaBuf})

	if err := p.wal.append(frames, p.sync); err != nil {
		return err
	}
	for _, fr := range frames {
		p.pending[fr.id] = fr.data
	}
	for _, n := range nodes {
		p.cache.put(n)
	}
	p.meta = m
	return nil
}

func (p *pager) needsCheckpoint() bool { return len(p.pending) >= p.checkpointPages }

// checkpoint copies the pending pages into the main file, syncs it, and
// only then empties the WAL. A crash halfway is safe: the WAL still has
// every page, and replaying a full page image twice gives the same page.
func (p *pager) checkpoint() error {
	if len(p.pending) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(p.pending))
	for id := range p.pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for i, id := range ids {
		if testCheckpointHook != nil {
			if err := testCheckpointHook(i); err != nil {
				return err
			}
		}
		if _, err := p.file.WriteAt(p.pending[id], int64(id)*PageSize); err != nil {
			return fmt.Errorf("checkpoint: %w", err)
		}
	}
	if err := p.file.Sync(); err != nil {
		return fmt.Errorf("checkpoint sync: %w", err)
	}
	if err := p.wal.reset(); err != nil {
		return err
	}
	p.pending = map[uint64][]byte{}
	return nil
}

func (p *pager) close() error {
	err1 := p.file.Close()
	err2 := p.wal.f.Close()
	if err1 != nil {
		return err1
	}
	return err2
}
