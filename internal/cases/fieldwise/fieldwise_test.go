package fieldwise_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/darxnet/gocopy/internal/cases/fieldwise"
)

func TestStateCopyFrom(t *testing.T) {
	t.Parallel()

	src := &fieldwise.State{
		Data:     []byte("data"),
		M:        map[string]int{"k": 1},
		Inner:    fieldwise.Inner{B: []byte("inner")},
		InnerPtr: &fieldwise.Inner{B: []byte("ptr")},
		Arr:      [2][]byte{[]byte("a0"), []byte("a1")},
		Inners:   [2]fieldwise.Inner{{B: []byte("i0")}, {B: []byte("i1")}},
		N:        3,
		S:        "s",
		Keep:     []byte("src keep"),
	}
	src.Count.Store(9)
	src.Stats.N = 9

	dst := fieldwise.State{
		Data: make([]byte, 0, 16),
		Keep: []byte("dst keep"),
	}
	dst.Count.Store(5)
	dst.Stats.N = 5
	dst.Mu.Lock()

	dst.CopyFrom(src)

	if dst.Mu.TryLock() {
		t.Fatal("Mu must be left as it was")
	}

	dst.Mu.Unlock()

	if dst.Count.Load() != 5 || dst.Stats.N != 5 || string(dst.Keep) != "dst keep" {
		t.Fatal("fields tagged copy:\"-\" must keep dst's value")
	}

	if !slices.Equal(dst.Data, src.Data) || cap(dst.Data) != 16 || !maps.Equal(dst.M, src.M) ||
		!slices.Equal(dst.Inner.B, src.Inner.B) || !slices.Equal(dst.InnerPtr.B, src.InnerPtr.B) ||
		!slices.Equal(dst.Arr[1], src.Arr[1]) || !slices.Equal(dst.Inners[1].B, src.Inners[1].B) ||
		dst.N != src.N || dst.S != src.S {
		t.Fatalf("dst = %+v, want the content of %+v", &dst, src)
	}

	src.Data[0] = 'X'
	src.M["k"] = 99
	src.Inner.B[0] = 'X'
	src.InnerPtr.B[0] = 'X'
	src.Arr[0][0] = 'X'
	src.Inners[0].B[0] = 'X'

	if string(dst.Data) != "data" || dst.M["k"] != 1 || string(dst.Inner.B) != "inner" ||
		string(dst.InnerPtr.B) != "ptr" || string(dst.Arr[0]) != "a0" || string(dst.Inners[0].B) != "i0" {
		t.Fatal("dst changed with src")
	}

	dst.CopyFrom(&fieldwise.State{})

	if len(dst.M) != 0 || dst.InnerPtr != nil || len(dst.Data) != 0 {
		t.Fatal("copying a zero src must clear the deep fields")
	}
}
