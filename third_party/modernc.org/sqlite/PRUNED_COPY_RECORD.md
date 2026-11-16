# Pruned copy of modernc.org/sqlite

MMWatchdog builds with this copy of `modernc.org/sqlite` instead of the module from
the Go module proxy: the `replace` line in MMWatchdog's `go.mod` points here. The
copy keeps only what MMWatchdog uses. [MMWatchdog's implementation
rules](../../../docs/changing-mmwatchdog/implementation-rules.md#go-dependencies) give the rule
for changing it.

## Upstream

- Module: `modernc.org/sqlite`, version `v1.59.0`.
- Module hash: `h1:X1es1GpqBlS/5T+vbM4HLUdaa8OtQx468DF2vrx+38A=`, as the Go
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

Kept: 139 of 597 Go files, 396,236 of 1,535,424 lines.

Kept packages:

- `modernc.org/sqlite`
- `modernc.org/sqlite/lib`
- `modernc.org/sqlite/vtab`

## Changed files

- `go.mod`: the requirements that no kept package needs are cut:
  `github.com/dustin/go-humanize`, `github.com/google/pprof`,
  `github.com/google/uuid`, `github.com/mattn/go-isatty`,
  `github.com/ncruces/go-strftime`, `github.com/remyoudompheng/bigfft`,
  `golang.org/x/sys`, `modernc.org/fileutil`, `modernc.org/mathutil`,
  `modernc.org/memory`.

## Applied fixes

None.
