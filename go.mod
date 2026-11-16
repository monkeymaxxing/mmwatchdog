module mmwatchdog

go 1.27.0

toolchain go1.27.4

require (
	go.starlark.net v0.0.0-20260908191801-89a6a09411d5
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/net v0.59.0
	golang.org/x/sys v0.48.0
	modernc.org/sqlite v1.59.0
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

// MMWatchdog builds with pruned copies of its dependencies. The implementation
// rules, docs/changing-mmwatchdog/implementation-rules.md section "Go dependencies", say why
// and when a copy may change.
replace (
	github.com/google/uuid => ./third_party/github.com/google/uuid
	github.com/mattn/go-isatty => ./third_party/github.com/mattn/go-isatty
	github.com/ncruces/go-strftime => ./third_party/github.com/ncruces/go-strftime
	go.starlark.net => ./third_party/go.starlark.net
	go.yaml.in/yaml/v3 => ./third_party/go.yaml.in/yaml/v3
	golang.org/x/net => ./third_party/golang.org/x/net
	golang.org/x/sys => ./third_party/golang.org/x/sys
	modernc.org/libc => ./third_party/modernc.org/libc
	modernc.org/mathutil => ./third_party/modernc.org/mathutil
	modernc.org/memory => ./third_party/modernc.org/memory
	modernc.org/sqlite => ./third_party/modernc.org/sqlite
)
