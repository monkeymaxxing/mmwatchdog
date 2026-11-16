# Pruned copy of golang.org/x/net

MMWatchdog builds with this copy of `golang.org/x/net` instead of the module from
the Go module proxy: the `replace` line in MMWatchdog's `go.mod` points here. The
copy keeps only what MMWatchdog uses. [MMWatchdog's implementation
rules](../../../../docs/changing-mmwatchdog/implementation-rules.md#go-dependencies) give the
rule for changing it.

## Upstream

- Module: `golang.org/x/net`, version `v0.59.0`.
- Module hash: `h1:5zfYln+w5XCxwrnMMJPufRgNoXEaGxl0wo5GqPXyues=`, as the Go
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

Kept: 2 of 742 Go files, 3,066 of 193,184 lines.

Kept packages:

- `golang.org/x/net/dns/dnsmessage`

## Changed files

- `go.mod`: the requirements that no kept package needs are cut:
  `golang.org/x/crypto`, `golang.org/x/sys`, `golang.org/x/term`,
  `golang.org/x/text`.

## Applied fixes

None.
