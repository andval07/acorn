package acorn

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// Page 0 holds the meta record. It goes through the WAL like any other
// page and is always the last page of a transaction, so a commit swaps
// the whole tree state (root, page count, free list) at once.
//
//	0..8    magic "ACORNDB1"
//	8..12   page size
//	12..20  root page
//	20..28  page count
//	28..36  head of the free list (0 = empty)
//	36..40  CRC-32 of bytes 0..36
const magic = "ACORNDB1"

type meta struct {
	root      uint64
	pageCount uint64
	freeHead  uint64
}

func (m meta) encode(buf []byte) {
	clear(buf)
	copy(buf, magic)
	binary.LittleEndian.PutUint32(buf[8:], PageSize)
	binary.LittleEndian.PutUint64(buf[12:], m.root)
	binary.LittleEndian.PutUint64(buf[20:], m.pageCount)
	binary.LittleEndian.PutUint64(buf[28:], m.freeHead)
	binary.LittleEndian.PutUint32(buf[36:], crc32.ChecksumIEEE(buf[:36]))
}

func decodeMeta(buf []byte) (meta, error) {
	if string(buf[:8]) != magic {
		return meta{}, fmt.Errorf("%w: not an acorn file", ErrCorrupt)
	}
	if crc32.ChecksumIEEE(buf[:36]) != binary.LittleEndian.Uint32(buf[36:]) {
		return meta{}, fmt.Errorf("%w: bad meta checksum", ErrCorrupt)
	}
	if size := binary.LittleEndian.Uint32(buf[8:]); size != PageSize {
		return meta{}, fmt.Errorf("%w: page size %d, want %d", ErrCorrupt, size, PageSize)
	}
	m := meta{
		root:      binary.LittleEndian.Uint64(buf[12:]),
		pageCount: binary.LittleEndian.Uint64(buf[20:]),
		freeHead:  binary.LittleEndian.Uint64(buf[28:]),
	}
	if m.root == 0 || m.root >= m.pageCount || m.freeHead >= m.pageCount {
		return meta{}, fmt.Errorf("%w: meta points outside the file", ErrCorrupt)
	}
	return m, nil
}
