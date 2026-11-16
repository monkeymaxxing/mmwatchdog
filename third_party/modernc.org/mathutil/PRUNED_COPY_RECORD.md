# Pruned copy of modernc.org/mathutil

MMWatchdog builds with this copy of `modernc.org/mathutil` instead of the module
from the Go module proxy: the `replace` line in MMWatchdog's `go.mod` points here.
The copy keeps only what MMWatchdog uses. [MMWatchdog's implementation
rules](../../../docs/changing-mmwatchdog/implementation-rules.md#go-dependencies) give the rule
for changing it.

## Upstream

- Module: `modernc.org/mathutil`, version `v1.7.1`.
- Module hash: `h1:GCZVGXdaN8gTqB1Mf/usp1Y/hSqgI2vAGGP4jZMCxOU=`, as the Go
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

Kept: 4 of 26 Go files, 9,087 of 18,715 lines.

Kept packages:

- `modernc.org/mathutil`

## Changed files

- `go.mod`: the requirements that no kept package needs are cut:
  `github.com/remyoudompheng/bigfft`.

## Applied fixes

None.
