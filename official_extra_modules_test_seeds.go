// Package mmwatchdog carries the shipped official extra modules as test
// seeds. The module implementations remain Starlark; MMWatchdog does not
// import this package. It sits at the repository root, not in
// official_extra_modules/, because the playbooks install that directory on
// every node, and a Go file has no place there.
package mmwatchdog

import (
	"embed"
	"io/fs"
)

// ModuleTestSeeds lets compiled tests exercise the shipped modules without a
// source checkout on the guest. It embeds the source files, not copied fixtures.
//
//go:embed official_extra_modules/*/module.star official_extra_modules/*/module_description.yml official_extra_modules/*/README.md official_extra_modules/shipped_with_mmwatchdog_version
var embeddedModules embed.FS

// ModuleTestSeeds holds the files with the same paths as in
// official_extra_modules/.
var ModuleTestSeeds = func() fs.FS {
	seeds, e := fs.Sub(embeddedModules, "official_extra_modules")
	if e != nil {
		panic(e)
	}
	return seeds
}()
