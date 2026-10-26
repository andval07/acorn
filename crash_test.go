package acorn

import (
	"bufio"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// history runs a few random transactions with checkpoints turned off, so
// everything stays in the WAL, then leaves the files as a crash would.
// It returns the state after each commit and the WAL size at that point.
func history(t *testing.T, path string, commits int) (states []map[string]string, ends []int64) {
	t.Helper()
	// Start from a checkpointed base, so the WAL holds only what follows.
	db, err := Open(path, fast)
	if err != nil {
		t.Fatal(err)
	}
	model := map[string]string{}
	for i := range 300 {
		k := fmt.Sprintf("base%04d", i)
		model[k] = "base"
		if err := db.Put([]byte(k), []byte("base")); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path, Options{NoSync: true, CheckpointPages: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	states = append(states, maps.Clone(model))
	ends = append(ends, 0)
	rng := rand.New(rand.NewPCG(7, 7))
	for c := range commits {
		err := db.Update(func(tx *Tx) error {
			for range rng.IntN(30) + 1 {
				k := fmt.Sprintf("base%04d", rng.IntN(400))
				if rng.IntN(3) == 0 {
					delete(model, k)
					if err := tx.Delete([]byte(k)); err != nil {
						return err
					}
					continue
				}
				v := fmt.Sprintf("c%d-%s", c, strings.Repeat("x", rng.IntN(200)))
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
		info, err := os.Stat(path + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, maps.Clone(model))
		ends = append(ends, info.Size())
	}
	db.abandon()
	return states, ends
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverWithoutCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	states, _ := history(t, path, 10)
	db, err := Open(path, fast)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Check(); err != nil {
		t.Fatal(err)
	}
	compare(t, db, states[len(states)-1])
}

// TestTornWAL cuts the WAL at many lengths, as a crash in the middle of a
// write would. After reopening, the database must hold exactly the last
// transaction whose frames are all inside the cut, never a mix.
func TestTornWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	states, ends := history(t, path, 12)
	mainFile, log := readFile(t, path), readFile(t, path+"-wal")

	lengths := map[int64]bool{}
	for _, e := range ends {
		for _, d := range []int64{-1, 0, 1, frameHeader} {
			if l := e + d; l >= 0 && l <= int64(len(log)) {
				lengths[l] = true
			}
		}
	}
	for l := int64(0); l <= int64(len(log)); l += frameSize / 3 {
		lengths[l] = true
	}

	for l := range lengths {
		want := 0
		for i, e := range ends {
			if e <= l {
				want = i
			}
		}
		writeFile(t, path, mainFile)
		writeFile(t, path+"-wal", log[:l])
		db, err := Open(path, fast)
		if err != nil {
			t.Fatalf("cut at %d: %v", l, err)
		}
		if err := db.Check(); err != nil {
			t.Fatalf("cut at %d: %v", l, err)
		}
		compare(t, db, states[want])
		db.Close()
	}
	t.Logf("%d cut points over %d commits", len(lengths), len(ends)-1)
}

// A damaged byte in the last transaction must drop that transaction only.
func TestCorruptLastFrame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	states, ends := history(t, path, 5)
	log := readFile(t, path+"-wal")
	last := len(ends) - 1
	log[ends[last-1]+frameHeader+100] ^= 0xff // inside the first page of the last commit
	writeFile(t, path+"-wal", log)

	db, err := Open(path, fast)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	compare(t, db, states[last-1])
}

// A crash halfway through a checkpoint leaves the main file with some new
// pages and some old. The WAL is only emptied after the main file is
// synced, so replaying it on open must fix the file.
func TestCrashDuringCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	states, _ := history(t, path, 8)
	mainFile, log := readFile(t, path), readFile(t, path+"-wal")

	// Count the pages one checkpoint copies, on a copy of the files.
	pages := 0
	testCheckpointHook = func(int) error { pages++; return nil }
	writeFile(t, path+"-count", mainFile)
	writeFile(t, path+"-count-wal", log)
	db, err := Open(path+"-count", fast)
	testCheckpointHook = nil
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Stop before the first page, the second, the middle one, the last.
	for _, stopAt := range []int{0, 1, pages / 2, pages - 1} {
		t.Run(fmt.Sprintf("after%dof%d", stopAt, pages), func(t *testing.T) {
			writeFile(t, path, mainFile)
			writeFile(t, path+"-wal", log)
			crash := errors.New("simulated crash")
			testCheckpointHook = func(copied int) error {
				if copied == stopAt {
					return crash
				}
				return nil
			}
			db, err := Open(path, fast) // recovery starts a checkpoint
			testCheckpointHook = nil
			if !errors.Is(err, crash) {
				if err == nil {
					db.Close()
				}
				t.Fatalf("got %v, want the simulated crash", err)
			}

			db, err = Open(path, fast)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.Check(); err != nil {
				t.Fatal(err)
			}
			compare(t, db, states[len(states)-1])
		})
	}
}

// A checkpoint that fails after a commit must not lose the commit, and
// must stop further writes until the database is reopened.
func TestCheckpointFailurePoisons(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, err := Open(path, Options{NoSync: true, CheckpointPages: 1})
	if err != nil {
		t.Fatal(err)
	}
	testCheckpointHook = func(int) error { return errors.New("disk gone") }
	err = db.Put([]byte("k"), []byte("v"))
	testCheckpointHook = nil
	if err != nil {
		t.Fatalf("commit reported %v, but it is safe in the WAL", err)
	}
	if err := db.Put([]byte("k2"), nil); err == nil {
		t.Fatal("write after a failed checkpoint was accepted")
	}
	if v, err := db.Get([]byte("k")); err != nil || string(v) != "v" {
		t.Fatalf("read after failure: %q, %v", v, err)
	}
	db.abandon()

	db, err = Open(path, fast)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, err := db.Get([]byte("k")); err != nil || string(v) != "v" {
		t.Fatalf("after reopen: %q, %v", v, err)
	}
}

// TestKillProcess runs a child process that commits with fsync on and
// prints each acknowledged commit, then kills it at a random moment.
// Every acknowledged commit must survive, and the file must be valid.
// This tests a process crash; a power cut also drops the OS cache, which
// only fsync protects against and which a test cannot reproduce.
func TestKillProcess(t *testing.T) {
	if path := os.Getenv("ACORN_CRASH_CHILD"); path != "" {
		crashChild(path)
		return
	}
	if testing.Short() {
		t.Skip("slow")
	}
	for run := range 3 {
		path := filepath.Join(t.TempDir(), "db")
		cmd := exec.Command(os.Args[0], "-test.run=^TestKillProcess$")
		cmd.Env = append(os.Environ(), "ACORN_CRASH_CHILD="+path)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}

		stopAfter := 50 + rand.IntN(200)
		acked := -1
		lines := bufio.NewScanner(out)
		for lines.Scan() {
			n, err := strconv.Atoi(strings.TrimPrefix(lines.Text(), "ok "))
			if err != nil {
				continue // other output from the test binary
			}
			acked = n
			if acked >= stopAfter {
				break
			}
		}
		cmd.Process.Kill()
		cmd.Wait()
		if acked < 0 {
			t.Fatal("child never acknowledged a commit")
		}

		db, err := Open(path, fast)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if err := db.Check(); err != nil {
			t.Fatal(err)
		}
		// The child may commit a few more times between the last line we
		// read and the kill. So: every acked commit must be there, and the
		// keys must be a prefix of the sequence, with no gaps.
		s, err := db.Stats()
		if err != nil {
			t.Fatal(err)
		}
		if s.Keys < acked+1 {
			t.Fatalf("run %d: %d keys after %d acked commits", run, s.Keys, acked+1)
		}
		for i := range s.Keys {
			if _, err := db.Get(crashKey(i)); err != nil {
				t.Fatalf("run %d: commit %d missing: %v", run, i, err)
			}
		}
		t.Logf("run %d: killed after %d acked commits, %d keys found", run, acked+1, s.Keys)
	}
}

func crashKey(i int) []byte { return fmt.Appendf(nil, "commit%08d", i) }

// crashChild commits forever, one key per transaction, until killed.
// A small checkpoint size makes the kill land in checkpoints too.
func crashChild(path string) {
	db, err := Open(path, Options{CheckpointPages: 20})
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	for i := 0; ; i++ {
		if err := db.Put(crashKey(i), make([]byte, 200)); err != nil {
			fmt.Println("put:", err)
			os.Exit(1)
		}
		fmt.Printf("ok %d\n", i)
	}
}
