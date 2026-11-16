module modernc.org/libc

go 1.25.0

retract v1.43.0

retract v1.67.5

retract v1.74.2 // freeaddrinfo leaks a locked ___lock entry; deadlocks name resolution, fixed in v1.74.4

retract v1.74.3 // freeaddrinfo leaks a locked ___lock entry; deadlocks name resolution, fixed in v1.74.4

require (
	github.com/google/uuid v1.6.0
	github.com/mattn/go-isatty v0.0.20
	github.com/ncruces/go-strftime v1.0.0
	golang.org/x/sys v0.47.0
	modernc.org/mathutil v1.7.1
	modernc.org/memory v1.12.1
)
