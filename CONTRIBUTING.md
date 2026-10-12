# Contributing

`gocopy` is maintained by one person; issues and pull requests are welcome.
Please open an issue first for anything bigger than a bug fix, so the design is
agreed before the code is written.

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org) are required: the
release version is computed from them by [`svu`](https://github.com/caarlos0/svu)
in CI when a commit lands on `main`.

| Prefix | Release |
|---|---|
| `fix:` | patch |
| `feat:` | minor |
| `feat!:` or a `BREAKING CHANGE:` footer | major |
| `chore:`, `docs:`, `test:`, `ci:`, `refactor:`, `perf:` | none |

A change in the generated code is a `feat:` (or `feat!:` when existing output
changes meaning): users regenerate and review the diff.

## Before a pull request

    go mod tidy
    go generate ./...                      # the committed test cases must be current
    go vet ./...
    go test -race -count=1 ./...
    golangci-lint run ./...                # .golangci.yml, `default: all`

CI runs the same checks on Go 1.24 through the latest release, on Linux and macOS,
plus `govulncheck` and a coverage gate.

## Ground rules

- Every new field kind or tag option comes with a field in a test case under
  `internal/cases` and an assertion on the generated code's behaviour, not
  only on its text.
- `TestGenerateMatchesCommitted` compares the generator with the files it
  produced for the test cases: a change in the output shows up there first.
- No dependencies: the module deliberately uses only the standard library.
  Packages are loaded with `go list -export` and `go/importer` (see `load.go`),
  not `golang.org/x/tools/go/packages`, so the Go version it needs is its own.
