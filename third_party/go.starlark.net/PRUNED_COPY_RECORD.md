# Pruned copy of go.starlark.net

MMWatchdog builds with this copy of `go.starlark.net` instead of the module from the
Go module proxy: the `replace` line in MMWatchdog's `go.mod` points here. The copy
keeps only what MMWatchdog uses. [MMWatchdog's implementation
rules](../../docs/changing-mmwatchdog/implementation-rules.md#go-dependencies) give the rule for
changing it.

## Upstream

- Module: `go.starlark.net`, version `v0.0.0-20260908191801-89a6a09411d5`.
- Module hash: `h1:X8HyonnLxrmAbdeMIEGEJVZ/yg6WykLZyAZmpCLSfMA=`, as the Go
  checksum database records it for this version.

## What was cut

The copy keeps the packages that MMWatchdog's programs and tests use. In each
package it keeps the files that a build for Linux on AMD64 or ARM64, or for
macOS on ARM64, with or without cgo, compiles and that MMWatchdog needs: the files
with a function that MMWatchdog reaches, the assembly files and the Go files that
declare their functions, and the files that declare a type, constant,
variable, or method that other kept code uses. `deadcode` from
golang.org/x/tools v0.52.0 measured which functions MMWatchdog reaches. Upstream
tests, examples, and tools are cut. Every kept file is identical to
upstream, except the files under "Changed files".

Kept: 24 of 54 Go files, 16,349 of 23,666 lines.

Kept packages:

- `go.starlark.net/internal/compile`
- `go.starlark.net/internal/spell`
- `go.starlark.net/lib/json`
- `go.starlark.net/resolve`
- `go.starlark.net/starlark`
- `go.starlark.net/starlarkstruct`
- `go.starlark.net/syntax`

## Changed files

- `go.mod`: the requirements that no kept package needs are cut:
  `github.com/chzyer/readline`, `github.com/google/go-cmp`,
  `golang.org/x/term`, `google.golang.org/protobuf`.

## Applied fixes

None.
