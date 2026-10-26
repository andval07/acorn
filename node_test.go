package acorn

import (
	"bytes"
	"errors"
	"testing"
)

func TestNodeRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		n    *node
	}{
		{"empty leaf", &node{id: 1, kind: kindLeaf}},
		{"leaf", &node{id: 2, kind: kindLeaf, link: 9,
			keys: [][]byte{[]byte("a"), []byte("bb")},
			vals: [][]byte{[]byte(""), []byte("value")}}},
		{"internal", &node{id: 3, kind: kindInternal,
			keys:     [][]byte{[]byte("m"), []byte("t")},
			children: []uint64{4, 5, 6}}},
		{"free", &node{id: 7, kind: kindFree, link: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := make([]byte, PageSize)
			tt.n.encode(buf)
			got, err := decode(tt.n.id, buf)
			if err != nil {
				t.Fatal(err)
			}
			if got.kind != tt.n.kind || got.link != tt.n.link || len(got.keys) != len(tt.n.keys) {
				t.Fatalf("got %+v, want %+v", got, tt.n)
			}
			for i := range got.keys {
				if !bytes.Equal(got.keys[i], tt.n.keys[i]) {
					t.Errorf("key %d: %q, want %q", i, got.keys[i], tt.n.keys[i])
				}
				if got.leaf() && !bytes.Equal(got.vals[i], tt.n.vals[i]) {
					t.Errorf("value %d: %q, want %q", i, got.vals[i], tt.n.vals[i])
				}
			}
			for i := range got.children {
				if got.children[i] != tt.n.children[i] {
					t.Errorf("child %d: %d, want %d", i, got.children[i], tt.n.children[i])
				}
			}
		})
	}
}

// A small cell that ends exactly at the end of the page is valid.
func TestNodeFullPage(t *testing.T) {
	n := &node{id: 1, kind: kindLeaf}
	for i := 0; ; i++ {
		k := []byte{byte(i >> 8), byte(i)}
		n.keys = append(n.keys, k)
		n.vals = append(n.vals, nil)
		if n.size() > PageSize {
			n.keys, n.vals = n.keys[:i], n.vals[:i]
			break
		}
	}
	// Grow the last value until the page is full to the byte.
	n.vals[len(n.vals)-1] = make([]byte, PageSize-n.size())
	if n.size() != PageSize {
		t.Fatalf("size %d", n.size())
	}
	buf := make([]byte, PageSize)
	n.encode(buf)
	if _, err := decode(1, buf); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeCorrupt(t *testing.T) {
	good := make([]byte, PageSize)
	(&node{id: 1, kind: kindLeaf, keys: [][]byte{[]byte("k")}, vals: [][]byte{[]byte("v")}}).encode(good)

	tests := []struct {
		name    string
		corrupt func(b []byte)
	}{
		{"unknown kind", func(b []byte) { b[0] = 9 }},
		{"huge count", func(b []byte) { b[2], b[3] = 0xff, 0xff }},
		{"offset past end", func(b []byte) { b[headerSize], b[headerSize+1] = 0xff, 0x0f }},
		{"key length past end", func(b []byte) {
			off := int(b[headerSize]) | int(b[headerSize+1])<<8
			b[off], b[off+1] = 0xff, 0xff
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := bytes.Clone(good)
			tt.corrupt(b)
			if _, err := decode(1, b); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("got %v, want ErrCorrupt", err)
			}
		})
	}
}
