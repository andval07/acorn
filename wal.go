package acorn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// The write-ahead log is a list of frames, each a full copy of one page:
//
//	0..8    page id
//	8..12   flags (1 = last frame of a transaction)
//	12..16  CRC-32 of bytes 0..12 and of the page
//	16..    the page
//
// A transaction is committed once its last frame (always the meta page,
// marked with the commit flag) is on disk. Recovery replays frames up to
// the last complete commit and ignores the rest: a crash in the middle
// of a commit leaves a torn tail, and the CRC catches it.
const (
	frameHeader = 16
	frameSize   = frameHeader + PageSize
	flagCommit  = 1
)

type wal struct {
	f   *os.File
	off int64 // where the next frame goes
}

type frame struct {
	id   uint64
	data []byte
}

// append writes a whole transaction with one write call. The offset only
// moves forward once the write (and the sync, when asked) succeeded, so a
// failed commit is overwritten by the next one.
func (w *wal) append(frames []frame, sync bool) error {
	buf := make([]byte, len(frames)*frameSize)
	for i, fr := range frames {
		b := buf[i*frameSize : (i+1)*frameSize]
		binary.LittleEndian.PutUint64(b[0:], fr.id)
		if i == len(frames)-1 {
			binary.LittleEndian.PutUint32(b[8:], flagCommit)
		}
		copy(b[frameHeader:], fr.data)
		binary.LittleEndian.PutUint32(b[12:], frameChecksum(b))
	}
	if _, err := w.f.WriteAt(buf, w.off); err != nil {
		return fmt.Errorf("write wal: %w", err)
	}
	if sync {
		if err := w.f.Sync(); err != nil {
			return fmt.Errorf("sync wal: %w", err)
		}
	}
	w.off += int64(len(buf))
	return nil
}

func frameChecksum(b []byte) uint32 {
	h := crc32.NewIEEE()
	h.Write(b[:12])
	h.Write(b[frameHeader:])
	return h.Sum32()
}

// recover returns the newest copy of every page written by a committed
// transaction, and sets the write offset just after the last commit.
func (w *wal) recover() (map[uint64][]byte, error) {
	committed := map[uint64][]byte{}
	pending := map[uint64][]byte{}
	var off int64
	for {
		b := make([]byte, frameSize)
		if _, err := w.f.ReadAt(b, off); err != nil {
			if errors.Is(err, io.EOF) {
				break // end of log, or a frame cut short by a crash
			}
			return nil, fmt.Errorf("read wal: %w", err)
		}
		if frameChecksum(b) != binary.LittleEndian.Uint32(b[12:]) {
			break // torn or damaged frame: nothing after it counts
		}
		pending[binary.LittleEndian.Uint64(b[0:])] = b[frameHeader:]
		off += frameSize
		if binary.LittleEndian.Uint32(b[8:])&flagCommit != 0 {
			for id, data := range pending {
				committed[id] = data
			}
			clear(pending)
			w.off = off
		}
	}
	return committed, nil
}

// reset empties the log after its pages were copied into the main file.
func (w *wal) reset() error {
	if err := w.f.Truncate(0); err != nil {
		return fmt.Errorf("truncate wal: %w", err)
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("sync wal: %w", err)
	}
	w.off = 0
	return nil
}
