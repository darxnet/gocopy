// Package fieldwise is a gocopy test case: a struct that cannot be assigned as
// a whole (it holds a lock, an atomic and a struct with a lock), so every
// field is copied on its own and there is no Copy method.
package fieldwise

import (
	"sync"
	"sync/atomic"
)

//go:generate go run ../../../cmd/gocopy -type=State

type Inner struct {
	B []byte
}

func (dst *Inner) CopyFrom(src *Inner) {
	dst.B = append(dst.B[:0], src.B...)
}

// Stats holds a lock by value: a field of this type needs a tag.
type Stats struct {
	Mu sync.Mutex
	N  int
}

type State struct {
	Mu       sync.Mutex   `copy:"-"`
	Count    atomic.Int64 `copy:"-"`
	Stats    Stats        `copy:"-"`
	Data     []byte
	M        map[string]int
	Inner    Inner
	InnerPtr *Inner
	Arr      [2][]byte
	Inners   [2]Inner
	N        int
	S        string
	Keep     []byte `copy:"-"`
}

// Untagged is here for the error path of Generate: a lock without a tag.
type Untagged struct {
	Mu sync.Mutex
}

// ArrayOfMaps is here for the error path of Generate.
type ArrayOfMaps struct {
	M [2]map[string]int
}
