# Pruned copy of go.yaml.in/yaml/v3

MMWatchdog builds with this copy of `go.yaml.in/yaml/v3` instead of the module from
the Go module proxy: the `replace` line in MMWatchdog's `go.mod` points here. The
copy keeps only what MMWatchdog uses. [MMWatchdog's implementation
rules](../../../../docs/changing-mmwatchdog/implementation-rules.md#go-dependencies) give the
rule for changing it.

## Upstream

- Module: `go.yaml.in/yaml/v3`, version `v3.0.5`.
- Module hash: `h1:N6y/pJk8buWs9NY5ERU2HSMfm+IuD/OtfdAnq6kESPw=`, as the Go
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

Kept: 13 of 18 Go files, 11,346 of 17,456 lines.

Kept packages:

- `go.yaml.in/yaml/v3`

## Changed files

None.

## Applied fixes

None.
