// Command bench compares acorn with SQLite on the same workloads and
// prints a Markdown table. Both run with full fsync on commit.
//
// SQLite here is modernc.org/sqlite, a pure Go translation of the C
// library (no cgo). It is slower than the C build, so the numbers are a
// comparison with that driver, not with SQLite at its fastest.
package main

import (
	"database/sql"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"time"

	"acorn"

	_ "modernc.org/sqlite"
)

const valueSize = 100

// store is the small surface both engines implement for the benchmark.
type store interface {
	insertAll(keys [][]byte) error // one transaction
	get(key []byte) error
	scanAll() (int, error)
	putCommit(key []byte) error // one transaction per key
	close() error
}

func main() {
	n := flag.Int("n", 100_000, "keys per workload")
	commits := flag.Int("commits", 1000, "small synced commits")
	runs := flag.Int("runs", 3, "runs per workload; the median is reported")
	flag.Parse()

	dir, err := os.MkdirTemp("", "acorn-bench")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(dir)

	engines := []struct {
		name string
		open func(path string) (store, error)
	}{
		{"acorn", openAcorn},
		{"SQLite (modernc, WAL, synchronous=FULL)", openSQLite},
	}
	workloads := []struct {
		name string
		unit string
		ops  int
		run  func(s store, keys [][]byte) error
		pre  bool // needs the keys loaded first
	}{
		{"insert, sequential keys, 1 tx", "keys/s", *n, func(s store, keys [][]byte) error {
			return s.insertAll(sorted(keys))
		}, false},
		{"insert, random keys, 1 tx", "keys/s", *n, func(s store, keys [][]byte) error {
			return s.insertAll(keys)
		}, false},
		{"get, random keys", "ops/s", *n, func(s store, keys [][]byte) error {
			for _, k := range keys {
				if err := s.get(k); err != nil {
					return err
				}
			}
			return nil
		}, true},
		{"full range scan", "keys/s", *n, func(s store, _ [][]byte) error {
			got, err := s.scanAll()
			if err == nil && got != *n {
				err = fmt.Errorf("scan returned %d keys", got)
			}
			return err
		}, true},
		{fmt.Sprintf("%d commits of 1 key, fsync each", *commits), "commits/s", *commits,
			func(s store, keys [][]byte) error {
				for _, k := range keys[:*commits] {
					if err := s.putCommit(k); err != nil {
						return err
					}
				}
				return nil
			}, false},
	}

	keys := randomKeys(*n)
	fmt.Printf("%d keys of 16 bytes, values of %d bytes, median of %d runs\n\n", *n, valueSize, *runs)
	fmt.Printf("| workload | %s | %s |\n|---|---:|---:|\n", engines[0].name, engines[1].name)
	for wi, w := range workloads {
		row := fmt.Sprintf("| %s |", w.name)
		for ei, e := range engines {
			var times []time.Duration
			for r := range *runs {
				path := filepath.Join(dir, fmt.Sprintf("w%d-e%d-r%d", wi, ei, r))
				s, err := e.open(path)
				if err != nil {
					fail(err)
				}
				if w.pre {
					if err := s.insertAll(keys); err != nil {
						fail(err)
					}
				}
				start := time.Now()
				if err := w.run(s, keys); err != nil {
					fail(fmt.Errorf("%s / %s: %w", e.name, w.name, err))
				}
				times = append(times, time.Since(start))
				if err := s.close(); err != nil {
					fail(err)
				}
			}
			slices.Sort(times)
			med := times[len(times)/2]
			row += fmt.Sprintf(" %s %s |", human(float64(w.ops)/med.Seconds()), w.unit)
		}
		fmt.Println(row)
	}
}

func randomKeys(n int) [][]byte {
	rng := rand.New(rand.NewPCG(1, 2))
	keys := make([][]byte, n)
	for i := range keys {
		k := make([]byte, 16)
		binary.BigEndian.PutUint64(k, rng.Uint64())
		binary.BigEndian.PutUint64(k[8:], uint64(i)) // unique
		keys[i] = k
	}
	return keys
}

func sorted(keys [][]byte) [][]byte {
	s := slices.Clone(keys)
	slices.SortFunc(s, func(a, b []byte) int { return slices.Compare(a, b) })
	return s
}

func human(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.2fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "bench:", err)
	os.Exit(1)
}

// acorn

type acornStore struct{ db *acorn.DB }

func openAcorn(path string) (store, error) {
	db, err := acorn.Open(path+".acorn", acorn.Options{})
	return &acornStore{db}, err
}

func (s *acornStore) insertAll(keys [][]byte) error {
	val := make([]byte, valueSize)
	return s.db.Update(func(tx *acorn.Tx) error {
		for _, k := range keys {
			if err := tx.Put(k, val); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *acornStore) get(key []byte) error {
	_, err := s.db.Get(key)
	return err
}

func (s *acornStore) scanAll() (int, error) {
	n := 0
	err := s.db.Scan(nil, nil, func(_, _ []byte) bool { n++; return true })
	return n, err
}

func (s *acornStore) putCommit(key []byte) error {
	return s.db.Put(key, make([]byte, valueSize))
}

func (s *acornStore) close() error { return s.db.Close() }

// SQLite

// Statements are prepared once, so the timing measures SQLite and not
// SQL parsing on every call.
type sqliteStore struct {
	db       *sql.DB
	getStmt  *sql.Stmt
	putStmt  *sql.Stmt
	scanStmt *sql.Stmt
}

func openSQLite(path string) (store, error) {
	db, err := sql.Open("sqlite", path+".sqlite")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one connection, so the pragmas apply to every query
	for _, q := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA cache_size=-16000", // 16 MB, about what acorn's node cache holds
		"CREATE TABLE kv (k BLOB PRIMARY KEY, v BLOB NOT NULL) WITHOUT ROWID",
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", q, err)
		}
	}
	s := &sqliteStore{db: db}
	for _, p := range []struct {
		stmt **sql.Stmt
		q    string
	}{
		{&s.getStmt, "SELECT v FROM kv WHERE k = ?"},
		{&s.putStmt, "INSERT OR REPLACE INTO kv (k, v) VALUES (?, ?)"},
		{&s.scanStmt, "SELECT k, v FROM kv ORDER BY k"},
	} {
		if *p.stmt, err = db.Prepare(p.q); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p.q, err)
		}
	}
	return s, nil
}

func (s *sqliteStore) insertAll(keys [][]byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt := tx.Stmt(s.putStmt)
	val := make([]byte, valueSize)
	for _, k := range keys {
		if _, err := stmt.Exec(k, val); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *sqliteStore) get(key []byte) error {
	var v []byte
	return s.getStmt.QueryRow(key).Scan(&v)
}

func (s *sqliteStore) scanAll() (int, error) {
	rows, err := s.scanStmt.Query()
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	var k, v []byte
	for rows.Next() {
		if err := rows.Scan(&k, &v); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func (s *sqliteStore) putCommit(key []byte) error {
	_, err := s.putStmt.Exec(key, make([]byte, valueSize))
	return err
}

func (s *sqliteStore) close() error { return s.db.Close() }
