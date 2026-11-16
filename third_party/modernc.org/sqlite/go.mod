module modernc.org/sqlite

go 1.25.0

require modernc.org/libc v1.75.7

retract [v1.16.0, v1.17.2] // https://gitlab.com/cznic/sqlite/-/issues/100

retract v1.19.0 // module source tree too large (max size is 524288000 bytes)

retract v1.20.1 // https://gitlab.com/cznic/sqlite/-/issues/123

retract v1.29.4 // tagged accidentally w/o builders checking the commit

retract v1.33.0 // intended to resolve #177 but breaks clients

retract v1.34.3 // intended to resolve #199 but breaks clients, see #200, fix in 1fcc86e9

retract v1.42.0 // Accidentaly broken, reverting to v1.41.0 state
