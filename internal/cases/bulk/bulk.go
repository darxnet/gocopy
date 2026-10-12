// Package bulk is a gocopy test case: a struct without locks, copied in one
// assignment with the sharing fields fixed after it.
package bulk

import "github.com/darxnet/gocopy/internal/cases/consts"

//go:generate go run ../../../cmd/gocopy -type=Request

// Inner has a hand-written CopyFrom, which the generator copies through.
type Inner struct {
	B []byte
	N int
}

func (dst *Inner) CopyFrom(src *Inner) {
	dst.B = append(dst.B[:0], src.B...)
	dst.N = src.N
}

// NotAStruct is here for the error path of Generate.
type NotAStruct int

type Request struct {
	Data     []byte
	Names    []string
	M        map[string]int
	Levels   map[string]consts.Level
	Inner    Inner
	InnerPtr *Inner
	Arr      [2][]byte
	Inners   [2]Inner
	Ptr      *int
	Any      any
	N        int
	S        string
	Keep     []byte `copy:"-"`
	Shared   []byte `copy:"shallow"`
}
