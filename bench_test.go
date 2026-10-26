package acorn

import (
	"encoding/binary"
	"math/rand/v2"
	"path/filepath"
	"testing"
)

// Keys are 16 bytes and values 100 bytes, the same as in bench/ for the
// comparison with SQLite.
const benchN = 100_000

func benchKey(i uint64) []byte {
	k := make([]byte, 16)
	binary.BigEndian.PutUint64(k, i)
	binary.BigEndian.PutUint64(k[8:], i*0x9E3779B97F4A7C15)
	return k
}

func fillDB(b *testing.B, db *DB, order []uint64) {
	val := make([]byte, 100)
	err := db.Update(func(tx *Tx) error {
		for _, i := range order {
			if err := tx.Put(benchKey(i), val); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
}

func openBench(b *testing.B) *DB {
	db, err := Open(filepath.Join(b.TempDir(), "bench.acorn"), Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	return db
}

func sequence(n int) []uint64 {
	s := make([]uint64, n)
	for i := range s {
		s[i] = uint64(i)
	}
	return s
}

func shuffled(n int) []uint64 {
	s := sequence(n)
	rand.New(rand.NewPCG(1, 2)).Shuffle(n, func(i, j int) { s[i], s[j] = s[j], s[i] })
	return s
}

func BenchmarkInsertSequential(b *testing.B) {
	for range b.N {
		fillDB(b, openBench(b), sequence(benchN))
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*benchN), "ns/key")
}

func BenchmarkInsertRandom(b *testing.B) {
	for range b.N {
		fillDB(b, openBench(b), shuffled(benchN))
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*benchN), "ns/key")
}

func BenchmarkGetRandom(b *testing.B) {
	db := openBench(b)
	fillDB(b, db, sequence(benchN))
	rng := rand.New(rand.NewPCG(3, 4))
	b.ResetTimer()
	for range b.N {
		if _, err := db.Get(benchKey(rng.Uint64N(benchN))); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScanAll(b *testing.B) {
	db := openBench(b)
	fillDB(b, db, sequence(benchN))
	b.ResetTimer()
	for range b.N {
		n := 0
		db.Scan(nil, nil, func(_, _ []byte) bool { n++; return true })
		if n != benchN {
			b.Fatalf("scanned %d", n)
		}
	}
}

// One small transaction per op, with fsync: this is bound by the disk.
func BenchmarkSyncedCommit(b *testing.B) {
	db := openBench(b)
	val := make([]byte, 100)
	b.ResetTimer()
	for i := range b.N {
		if err := db.Put(benchKey(uint64(i)), val); err != nil {
			b.Fatal(err)
		}
	}
}
