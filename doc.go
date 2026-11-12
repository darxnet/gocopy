// Package gocopy generates CopyFrom and Copy methods that make a struct value
// an independent copy of another without allocating what the destination
// already owns.
//
// A generated CopyFrom(src) is one bulk assignment (*dst = *src) followed by
// fixes for the fields that would otherwise share memory: slices are copied
// into dst's own backing arrays (reusing their capacity), maps entry by entry
// into dst's own map, nested values with a CopyFrom through it — at any depth:
// the elements of a [][]byte or the values of a map[string][]byte get their
// own copies too. Pointers, interfaces, channels and funcs are assigned as
// they are. Struct tags choose a different treatment per field:
//
//	Buf   []byte  `copy:"-"`        // dst keeps its own value
//	Table []byte  `copy:"shallow"`  // assigned as it is, on purpose
//
// A struct holding a lock or an atomic (also nested, by value) cannot be
// assigned as a whole: such fields must be tagged, the type is copied field
// by field, and it gets no Copy method. A lock is what go vet copylocks calls
// one (a type whose pointer is a sync.Locker and value is not, so a custom
// noCopy too) and any other sync or sync/atomic type except an interface.
//
// The usual way to run it is a go:generate directive next to the type:
//
//	//go:generate go tool gocopy -type=Request
//
// which writes request_copy.gen.go in the same package. Name all the types of
// a package in one -type list: a type sees the CopyFrom of another (or of
// itself) generated in the same run, so the output does not depend on their
// order or on the files already generated. [Load], [Package.Generate] and
// [Package.GenerateAll] expose the generator to other tools.
package gocopy
