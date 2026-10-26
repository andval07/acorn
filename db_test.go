package acorn

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func openTemp(t *testing.T, opts Options) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.acorn")
	db, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// fast skips fsync: these tests check logic, not durability.
var fast = Options{NoSync: true}

func TestEmpty(t *testing.T) {
	db, _ := openTemp(t, fast)
	if _, err := db.Get([]byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if err := db.Delete([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := db.Check(); err != nil {
		t.Fatal(err)
	}
}

func TestPutGetOverwrite(t *testing.T) {
	db, _ := openTemp(t, fast)
	key := []byte("key")
	for _, v := range []string{"one", "two", ""} {
		if err := db.Put(key, []byte(v)); err != nil {
			t.Fatal(err)
		}
		got, err := db.Get(key)
		if err != nil || string(got) != v {
			t.Fatalf("got %q, %v; want %q", got, err, v)
		}
	}
}

func TestLimits(t *testing.T) {
	db, _ := openTemp(t, fast)
	tests := []struct {
		name     string
		key, val []byte
		ok       bool
	}{
		{"empty key", nil, nil, false},
		{"max key", make([]byte, MaxKeySize), nil, true},
		{"key too long", make([]byte, MaxKeySize+1), nil, false},
		{"max entry", []byte("k"), make([]byte, MaxEntrySize-1), true},
		{"entry too big", []byte("k"), make([]byte, MaxEntrySize), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := db.Put(tt.key, tt.val)
			if tt.ok && err != nil {
				t.Fatal(err)
			}
			if !tt.ok && !errors.Is(err, ErrTooLarge) {
				t.Fatalf("got %v, want ErrTooLarge", err)
			}
		})
	}
}

func TestPutCopiesBuffers(t *testing.T) {
	db, _ := openTemp(t, fast)
	k, v := []byte("key"), []byte("value")
	if err := db.Put(k, v); err != nil {
		t.Fatal(err)
	}
	k[0], v[0] = 'X', 'X'
	got, err := db.Get([]byte("key"))
	if err != nil || string(got) != "value" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRollback(t *testing.T) {
	db, _ := openTemp(t, fast)
	boom := errors.New("boom")
	err := db.Update(func(tx *Tx) error {
		for i := range 500 {
			if err := tx.Put(fmt.Appendf(nil, "k%04d", i), make([]byte, 100)); err != nil {
				return err
			}
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	s, err := db.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if s.Keys != 0 || s.Pages != 2 {
		t.Fatalf("rollback left %+v", s)
	}
}

func TestReadOnlyAndDone(t *testing.T) {
	db, _ := openTemp(t, fast)
	var saved *Tx
	db.View(func(tx *Tx) error {
		saved = tx
		if err := tx.Put([]byte("k"), nil); !errors.Is(err, ErrReadOnly) {
			t.Errorf("Put in View: %v", err)
		}
		return nil
	})
	if _, err := saved.Get([]byte("k")); !errors.Is(err, ErrTxDone) {
		t.Errorf("Get after View: %v", err)
	}
}

func TestScanBounds(t *testing.T) {
	db, _ := openTemp(t, fast)
	db.Update(func(tx *Tx) error {
		for i := range 2000 {
			tx.Put(fmt.Appendf(nil, "%05d", i*2), []byte("v"))
		}
		return nil
	})
	collect := func(start, end []byte, limit int) []string {
		var out []string
		db.Scan(start, end, func(k, _ []byte) bool {
			out = append(out, string(k))
			return len(out) < limit
		})
		return out
	}
	if got := collect(nil, nil, 1<<30); len(got) != 2000 {
		t.Fatalf("full scan: %d keys", len(got))
	}
	// Bounds that fall between keys, start inclusive, end exclusive.
	got := collect([]byte("00999"), []byte("01006"), 100)
	if want := []string{"01000", "01002", "01004"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := collect([]byte("00100"), nil, 2); !slices.Equal(got, []string{"00100", "00102"}) {
		t.Fatalf("early stop: %v", got)
	}
	if got := collect([]byte("99999"), nil, 100); len(got) != 0 {
		t.Fatalf("past the end: %v", got)
	}
}

// TestRandomModel runs random puts and deletes against a Go map and
// checks the tree after every transaction. Values have random sizes so
// splits and merges happen with uneven cells, and the deletes are heavy
// enough that the tree grows and shrinks several times.
func TestRandomModel(t *testing.T) {
	for seed := range uint64(4) {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 1))
			db, path := openTemp(t, Options{NoSync: true, CacheNodes: 64, CheckpointPages: 50})
			model := map[string]string{}

			for round := range 300 {
				deleteBias := 0.3
				if round%100 >= 60 {
					deleteBias = 0.8 // shrink phase
				}
				err := db.Update(func(tx *Tx) error {
					for range rng.IntN(40) + 1 {
						k := fmt.Sprintf("key%05d", rng.IntN(3000))
						if rng.Float64() < deleteBias {
							delete(model, k)
							if err := tx.Delete([]byte(k)); err != nil {
								return err
							}
							continue
						}
						v := string(bytes.Repeat([]byte{byte('a' + rng.IntN(26))}, rng.IntN(300)))
						model[k] = v
						if err := tx.Put([]byte(k), []byte(v)); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Check(); err != nil {
					t.Fatalf("round %d: %v", round, err)
				}
				if round%50 == 49 {
					// Reopen to read everything back from disk.
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
					if db, err = Open(path, fast); err != nil {
						t.Fatal(err)
					}
				}
			}
			compare(t, db, model)
			db.Close()
		})
	}
}

// compare checks that db holds exactly the model, through Get and Scan.
func compare(t *testing.T, db *DB, model map[string]string) {
	t.Helper()
	for k, v := range model {
		got, err := db.Get([]byte(k))
		if err != nil || string(got) != v {
			t.Fatalf("Get(%s) = %d bytes, %v; want %d bytes", k, len(got), err, len(v))
		}
	}
	want := slices.Sorted(maps.Keys(model))
	var got []string
	db.Scan(nil, nil, func(k, _ []byte) bool {
		got = append(got, string(k))
		return true
	})
	if !slices.Equal(got, want) {
		t.Fatalf("scan: %d keys, want %d", len(got), len(want))
	}
}

// Deleting everything must return every page to the free list, and new
// inserts must reuse them instead of growing the file.
func TestPagesReused(t *testing.T) {
	db, _ := openTemp(t, fast)
	fill := func() {
		err := db.Update(func(tx *Tx) error {
			for i := range 5000 {
				if err := tx.Put(fmt.Appendf(nil, "%06d", i), make([]byte, 50)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	fill()
	before, _ := db.Stats()
	err := db.Update(func(tx *Tx) error {
		for i := range 5000 {
			if err := tx.Delete(fmt.Appendf(nil, "%06d", i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := db.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if empty.Keys != 0 || empty.Depth != 1 || empty.FreePages != empty.Pages-2 {
		t.Fatalf("after delete all: %+v", empty)
	}
	fill()
	after, _ := db.Stats()
	if after.Pages != before.Pages {
		t.Fatalf("file grew from %d to %d pages", before.Pages, after.Pages)
	}
}

// Keys inserted in order must fill pages well, not leave every leaf half
// empty. Values of different sizes make the split points uneven.
func TestSequentialFill(t *testing.T) {
	for _, valSize := range []int{10, 100, 900} {
		t.Run(fmt.Sprint("value", valSize), func(t *testing.T) {
			db, _ := openTemp(t, fast)
			const n = 20_000
			model := map[string]string{}
			data := 0
			for start := 0; start < n; start += 1000 {
				err := db.Update(func(tx *Tx) error {
					for i := start; i < start+1000; i++ {
						k := fmt.Sprintf("key%08d", i)
						v := strings.Repeat("v", 1+i%valSize)
						model[k] = v
						data += leafCellSize([]byte(k), []byte(v))
						if err := tx.Put([]byte(k), []byte(v)); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Check(); err != nil {
					t.Fatal(err)
				}
			}
			s, err := db.Stats()
			if err != nil {
				t.Fatal(err)
			}
			// Pages 0 (meta) and internal nodes count too, so this is a
			// lower bound on how full the leaves are.
			fill := float64(data) / float64((s.Pages-1)*PageSize)
			t.Logf("%d pages, fill %.0f%%", s.Pages, fill*100)
			if fill < 0.70 {
				t.Fatalf("pages only %.0f%% full after sequential insert", fill*100)
			}

			// The fuller pages must still merge and rebalance correctly.
			err = db.Update(func(tx *Tx) error {
				for i := 0; i < n; i += 3 {
					k := fmt.Sprintf("key%08d", i)
					delete(model, k)
					if err := tx.Delete([]byte(k)); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Check(); err != nil {
				t.Fatal(err)
			}
			compare(t, db, model)
		})
	}
}
