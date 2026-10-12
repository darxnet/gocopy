package bulk_test

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/darxnet/gocopy/internal/cases/bulk"
	"github.com/darxnet/gocopy/internal/cases/consts"
)

func newSource() *bulk.Request {
	n := 7

	return &bulk.Request{
		Data:     []byte("data"),
		Names:    []string{"a", "b"},
		M:        map[string]int{"k": 1},
		Levels:   map[string]consts.Level{"l": 2},
		Inner:    bulk.Inner{B: []byte("inner"), N: 1},
		InnerPtr: &bulk.Inner{B: []byte("ptr"), N: 2},
		Arr:      [2][]byte{[]byte("a0"), []byte("a1")},
		Inners:   [2]bulk.Inner{{B: []byte("i0")}, {B: []byte("i1")}},
		Ptr:      &n,
		Any:      "any",
		N:        3,
		S:        "s",
		Keep:     []byte("src keep"),
		Shared:   []byte("shared"),
	}
}

func TestRequestCopyFrom(t *testing.T) {
	t.Parallel()

	src := newSource()

	dst := bulk.Request{
		Data:     make([]byte, 0, 16),
		M:        map[string]int{"old": 9},
		InnerPtr: &bulk.Inner{B: make([]byte, 0, 16)},
		Keep:     []byte("dst keep"),
	}
	dstData := dst.Data[:1]
	dstMap := reflect.ValueOf(dst.M).Pointer()
	dstInnerPtr := dst.InnerPtr

	dst.CopyFrom(src)

	assertEqualContent(t, &dst, src)

	// dst's own memory is reused
	if cap(dst.Data) != 16 || &dst.Data[0] != &dstData[0] {
		t.Fatal("Data must be copied into dst's own array")
	}

	if reflect.ValueOf(dst.M).Pointer() != dstMap || dst.M["old"] != 0 || len(dst.M) != 1 {
		t.Fatalf("M = %v, want src's entries in dst's own map", dst.M)
	}

	if dst.InnerPtr != dstInnerPtr || cap(dst.InnerPtr.B) != 16 {
		t.Fatal("InnerPtr must copy into dst's own object")
	}

	if string(dst.Keep) != "dst keep" {
		t.Fatalf("Keep = %q, want dst's own", dst.Keep)
	}

	if &dst.Shared[0] != &src.Shared[0] {
		t.Fatal("Shared must share src's array")
	}

	// and nothing but Shared is shared with src
	src.Data[0] = 'X'
	src.Names[0] = "X"
	src.M["k"] = 99
	src.Levels["l"] = 99
	src.Inner.B[0] = 'X'
	src.InnerPtr.B[0] = 'X'
	src.Arr[0][0] = 'X'
	src.Inners[0].B[0] = 'X'

	if string(dst.Data) != "data" || dst.Names[0] != "a" || dst.M["k"] != 1 || dst.Levels["l"] != 2 ||
		string(dst.Inner.B) != "inner" || string(dst.InnerPtr.B) != "ptr" ||
		string(dst.Arr[0]) != "a0" || string(dst.Inners[0].B) != "i0" {
		t.Fatal("dst changed with src")
	}
}

func TestRequestCopyFromNil(t *testing.T) {
	t.Parallel()

	src := &bulk.Request{}
	dst := newSource()

	dst.CopyFrom(src)

	// dst keeps its maps (cleared) and slices (empty) for the next copy; a
	// pointer follows src and becomes nil
	if len(dst.M) != 0 || dst.M == nil || len(dst.Levels) != 0 || dst.InnerPtr != nil {
		t.Fatalf("M = %v, Levels = %v, InnerPtr = %v after copying a zero src", dst.M, dst.Levels, dst.InnerPtr)
	}

	if len(dst.Data) != 0 || len(dst.Names) != 0 || len(dst.Inner.B) != 0 || len(dst.Arr[0]) != 0 {
		t.Fatal("slices must be empty after copying a zero src")
	}

	var zero bulk.Request

	zero.CopyFrom(src)

	if zero.M != nil || zero.Data != nil || zero.InnerPtr != nil {
		t.Fatal("a zero dst copying a zero src must stay zero: nothing to allocate")
	}
}

func TestRequestCopy(t *testing.T) {
	t.Parallel()

	src := newSource()
	dst := src.Copy()

	assertEqualContent(t, &dst, src)

	src.Data[0] = 'X'

	if string(dst.Data) != "data" {
		t.Fatal("Copy must be independent of src")
	}

	if len(dst.Keep) != 0 {
		t.Fatalf("Keep = %q, want the zero dst's own value", dst.Keep)
	}
}

func assertEqualContent(t *testing.T, dst, src *bulk.Request) {
	t.Helper()

	if !slices.Equal(dst.Data, src.Data) || !slices.Equal(dst.Names, src.Names) ||
		!maps.Equal(dst.M, src.M) || !maps.Equal(dst.Levels, src.Levels) ||
		!slices.Equal(dst.Inner.B, src.Inner.B) || dst.Inner.N != src.Inner.N ||
		!slices.Equal(dst.InnerPtr.B, src.InnerPtr.B) || dst.InnerPtr.N != src.InnerPtr.N ||
		!slices.Equal(dst.Arr[0], src.Arr[0]) || !slices.Equal(dst.Arr[1], src.Arr[1]) ||
		!slices.Equal(dst.Inners[0].B, src.Inners[0].B) || !slices.Equal(dst.Inners[1].B, src.Inners[1].B) ||
		dst.Ptr != src.Ptr || dst.Any != src.Any || dst.N != src.N || dst.S != src.S {
		t.Fatalf("dst = %+v, want the content of %+v", dst, src)
	}
}
