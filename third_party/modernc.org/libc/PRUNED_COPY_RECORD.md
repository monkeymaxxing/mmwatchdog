# Pruned copy of modernc.org/libc

MMWatchdog builds with this copy of `modernc.org/libc` instead of the module from
the Go module proxy: the `replace` line in MMWatchdog's `go.mod` points here. The
copy keeps only what MMWatchdog uses. [MMWatchdog's implementation
rules](../../../docs/changing-mmwatchdog/implementation-rules.md#go-dependencies) give the rule
for changing it.

## Upstream

- Module: `modernc.org/libc`, version `v1.75.7`.
- Module hash: `h1:o3DTP9/0p9pKmY2WCKQaySW6wIiZhNM7wc2lUoyhfew=`, as the Go
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

Kept: 69 of 1,383 Go files, 403,859 of 2,178,784 lines.

Kept packages:

- `modernc.org/libc`
- `modernc.org/libc/errno`
- `modernc.org/libc/fcntl`
- `modernc.org/libc/fts`
- `modernc.org/libc/grp`
- `modernc.org/libc/honnef.co/go/netdb`
- `modernc.org/libc/langinfo`
- `modernc.org/libc/limits`
- `modernc.org/libc/netdb`
- `modernc.org/libc/netinet/in`
- `modernc.org/libc/poll`
- `modernc.org/libc/pthread`
- `modernc.org/libc/pwd`
- `modernc.org/libc/signal`
- `modernc.org/libc/stdio`
- `modernc.org/libc/stdlib`
- `modernc.org/libc/sys/socket`
- `modernc.org/libc/sys/stat`
- `modernc.org/libc/sys/types`
- `modernc.org/libc/termios`
- `modernc.org/libc/time`
- `modernc.org/libc/unistd`
- `modernc.org/libc/utime`
- `modernc.org/libc/uuid/uuid`
- `modernc.org/libc/wctype`

## Changed files

- `go.mod`: the requirements that no kept package needs are cut:
  `github.com/dustin/go-humanize`, `github.com/hashicorp/golang-lru/v2`,
  `github.com/remyoudompheng/bigfft`, `golang.org/x/mod`,
  `golang.org/x/sync`, `golang.org/x/tools`, `modernc.org/cc/v4`,
  `modernc.org/ccgo/v4`, `modernc.org/fileutil`, `modernc.org/gc/v2`,
  `modernc.org/gc/v3`, `modernc.org/goabi0`, `modernc.org/opt`,
  `modernc.org/sortutil`, `modernc.org/strutil`, `modernc.org/token`.

## Applied fixes

None.
