// Command gocopy writes a CopyFrom and Copy methods for the struct types named by -type
// into <type>_copy.gen.go in the current package.
//
//	//go:generate go tool gocopy -type=Request,Response
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/darxnet/gocopy"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run is main without the process: it returns the exit code.
func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("gocopy", flag.ContinueOnError)
	flags.SetOutput(stderr)

	typeFlag := flags.String("type", "", "comma-separated names of the struct types to generate CopyFrom for")
	dirFlag := flags.String("dir", ".", "directory of the package")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	say := func(format string, a ...any) {
		_, _ = fmt.Fprintf(stderr, format+"\n", a...)
	}

	if *typeFlag == "" {
		say("gocopy: -type is required")
		flags.Usage()

		return 2
	}

	pkg, err := gocopy.Load(*dirFlag)
	if err != nil {
		say("%v", err)

		return 1
	}

	var typeNames []string

	for typeName := range strings.SplitSeq(*typeFlag, ",") {
		if typeName = strings.TrimSpace(typeName); typeName != "" {
			typeNames = append(typeNames, typeName)
		}
	}

	// one run: the types see each other's CopyFrom before it is written
	srcs, err := pkg.GenerateAll(typeNames...)
	if err != nil {
		say("%v", err)

		return 1
	}

	for i, typeName := range typeNames {
		name := filepath.Join(*dirFlag, gocopy.FileName(typeName))

		if err := os.WriteFile(name, srcs[i], 0o600); err != nil {
			say("gocopy: write %s: %v", name, err)

			return 1
		}

		say("gocopy: wrote %s", name)
	}

	return 0
}
