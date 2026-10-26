// Command acorn is a small shell for an acorn database file.
//
//	acorn FILE put KEY VALUE
//	acorn FILE get KEY
//	acorn FILE del KEY
//	acorn FILE scan [START [END]]
//	acorn FILE stats
//	acorn FILE check
package main

import (
	"errors"
	"fmt"
	"os"

	"acorn"
)

const usage = `usage:
  acorn FILE put KEY VALUE
  acorn FILE get KEY
  acorn FILE del KEY
  acorn FILE scan [START [END]]
  acorn FILE stats
  acorn FILE check`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		return errors.New(usage)
	}
	path, cmd, rest := args[0], args[1], args[2:]
	want := map[string][2]int{ // min and max arguments
		"put": {2, 2}, "get": {1, 1}, "del": {1, 1},
		"scan": {0, 2}, "stats": {0, 0}, "check": {0, 0},
	}
	n, ok := want[cmd]
	if !ok || len(rest) < n[0] || len(rest) > n[1] {
		return errors.New(usage)
	}

	db, err := acorn.Open(path, acorn.Options{})
	if err != nil {
		return err
	}
	err = runCmd(db, cmd, rest)
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	return err
}

func runCmd(db *acorn.DB, cmd string, args []string) error {
	switch cmd {
	case "put":
		return db.Put([]byte(args[0]), []byte(args[1]))
	case "get":
		v, err := db.Get([]byte(args[0]))
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", v)
	case "del":
		return db.Delete([]byte(args[0]))
	case "scan":
		var start, end []byte
		if len(args) > 0 {
			start = []byte(args[0])
		}
		if len(args) > 1 {
			end = []byte(args[1])
		}
		return db.Scan(start, end, func(k, v []byte) bool {
			fmt.Printf("%s\t%s\n", k, v)
			return true
		})
	case "stats":
		s, err := db.Stats()
		if err != nil {
			return err
		}
		fmt.Printf("keys %d\ndepth %d\npages %d (%d free)\nsize %d bytes\n",
			s.Keys, s.Depth, s.Pages, s.FreePages, s.Pages*acorn.PageSize)
	case "check":
		if err := db.Check(); err != nil {
			return err
		}
		fmt.Println("ok")
	}
	return nil
}
