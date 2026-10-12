// Package gocopy generates CopyFrom and Copy methods that make a struct value
// an independent copy of another without allocating what the destination
// already owns.
//
// A generated CopyFrom(src) is one bulk assignment (*dst = *src) followed by
// fixes for the fields that would otherwise share memory: slices are copied
// into dst's own backing arrays (reusing their capacity), maps entry by entry
// into dst's own map, nested values with a CopyFrom through it. Pointers,
// interfaces, channels and funcs are assigned as they are. Struct tags choose
// a different treatment per field:
//
//	Buf   []byte  `copy:"-"`        // dst keeps its own value
//	Table []byte  `copy:"shallow"`  // assigned as it is, on purpose
//
// A struct holding a lock or an atomic (also nested, by value) cannot be
// assigned as a whole: such fields must be tagged, the type is copied field
// by field, and it gets no Copy method.
//
// The usual way to run it is a go:generate directive next to the type:
//
//	//go:generate go tool gocopy -type=Request
//
// which writes request_copy.gen.go in the same package. [Load] and
// [Package.Generate] expose the generator to other tools.
package gocopy
