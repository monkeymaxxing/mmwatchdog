package checksandalerts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"mmwatchdog/internal/primitives"
)

// A module has one of three kinds: a core module is built into this
// program, an official extra module is a Starlark script that the MMWatchdog
// project writes, and a module of the project's own is a Starlark script or
// an executable that the people who run MMWatchdog write. The modules that are not
// core live in two directories of the node, which every deploy installs.
// The rules are in docs/changing-mmwatchdog/design/checks.md under "Modules".

// Version is the version of this mmwatchdog program. The command line sets it from
// the build; it is "0.1.0-dev" in a build that is no release.
var Version = "0.1.0-dev"

// The kinds of a module, as status, "mmwatchdog describe-module", and
// "mmwatchdog module-usage" name them in JSON.
const (
	ModuleKindCore              = "core"
	ModuleKindOfficialExtra     = "official_extra"
	ModuleKindProjectStarlark   = "project_starlark"
	ModuleKindProjectExecutable = "project_executable"
)

const (
	// ShippedWithVersionFile is the file in the directory of the official
	// extra modules that holds the version of the MMWatchdog release that they
	// belong to.
	ShippedWithVersionFile = "shipped_with_mmwatchdog_version"
	// ModuleExecutableFile is the name of the executable of a module of the
	// project's own that is no Starlark script.
	ModuleExecutableFile = "module_executable"
)

// ModuleDirs are the two directories in which a node holds the modules that
// are not core: the official extra modules of the installed MMWatchdog release, and
// the modules of the project's own.
type ModuleDirs struct{ OfficialExtra, External string }

// ModuleCatalog knows which modules that are not core a node has. It lists
// the two directories when it is made, and reads a module's description
// only when something asks for that module: MMWatchdog loads only the modules that
// the node's configuration names.
type ModuleCatalog struct {
	Dirs ModuleDirs
	// OfficialExtra and external are the names of the modules in the two
	// directories, in order of name.
	OfficialExtra, External []string
	mu                      sync.Mutex
	read                    map[string]*InstalledModule
	problems                map[string]error
}

// InstalledModule is one module that is not core, as the node has it.
type InstalledModule struct {
	Name string
	// Kind is ModuleKindOfficialExtra, ModuleKindProjectStarlark, or
	// ModuleKindProjectExecutable.
	Kind string
	// Directory holds the module's files.
	Directory   string
	Description *ModuleDescription
}

// IsStarlark reports a module that is a Starlark script.
func (m *InstalledModule) IsStarlark() bool { return m.Kind != ModuleKindProjectExecutable }

// LoadModuleCatalog lists the modules of the two directories. A directory
// that does not exist holds no module. It refuses official extra modules
// that were shipped with another version of MMWatchdog than this program, a module
// that has the name of a core module, of a setting, or of an official extra
// module, and a directory whose name no module can have.
func LoadModuleCatalog(dirs ModuleDirs) (*ModuleCatalog, error) {
	c := &ModuleCatalog{Dirs: dirs, read: map[string]*InstalledModule{}, problems: map[string]error{}}
	var e error
	if c.OfficialExtra, e = moduleDirectoriesIn(dirs.OfficialExtra); e != nil {
		return nil, e
	}
	if len(c.OfficialExtra) > 0 || primitives.IsDirectory(dirs.OfficialExtra) {
		written, e := primitives.ReadFile(filepath.Join(dirs.OfficialExtra, ShippedWithVersionFile), 256)
		if errors.Is(e, os.ErrNotExist) {
			return nil, fmt.Errorf("%s has no file %s, so it is not the official_extra_modules directory of an MMWatchdog release: deploy an MMWatchdog release again, which installs that directory beside the mmwatchdog program", dirs.OfficialExtra, ShippedWithVersionFile)
		}
		if e != nil {
			return nil, e
		}
		if shippedWith := strings.TrimSpace(string(written)); shippedWith != Version {
			return nil, fmt.Errorf("the official extra modules in %s were shipped with MMWatchdog %s, and this mmwatchdog program is MMWatchdog %s. One MMWatchdog release holds both: deploy that release again", dirs.OfficialExtra, shippedWith, Version)
		}
	}
	if c.External, e = moduleDirectoriesIn(dirs.External); e != nil {
		return nil, e
	}
	for _, name := range c.OfficialExtra {
		if taken := nameTakenInMMWatchdog(name); taken != "" {
			return nil, fmt.Errorf("the official extra module %s in %s has the name of %s", name, dirs.OfficialExtra, taken)
		}
	}
	for _, name := range c.External {
		taken := nameTakenInMMWatchdog(name)
		if taken == "" && slices.Contains(c.OfficialExtra, name) {
			taken = "the official extra module " + name + ", which every deploy installs"
		}
		if taken != "" {
			return nil, fmt.Errorf("the module %s in %s has the name of %s. A name means one module on every node: give your module another name", name, dirs.External, taken)
		}
	}
	return c, nil
}

// moduleDirectoriesIn lists the modules of a directory: each directory in
// it is one module, under the directory's name. Files in it, such as a
// README.md, are no modules.
func moduleDirectoriesIn(directory string) ([]string, error) {
	entries, e := os.ReadDir(directory)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	names := []string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !ModuleNamePattern.MatchString(entry.Name()) {
			return nil, fmt.Errorf("%s is no module: the name of a module uses lower-case letters, digits, and underscores, starts with a letter, and has at most 64 characters, such as queued_jobs", filepath.Join(directory, entry.Name()))
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

// nameTakenInMMWatchdog says what in MMWatchdog itself has a name, or "": a core module, or
// a setting of a check, a check group, or a destination.
func nameTakenInMMWatchdog(name string) string {
	if CheckModulesByName[name] != nil || DestinationModulesByName[name] != nil {
		return "the core module " + name
	}
	if slices.Contains(SettingNamesBesideAModule(), name) {
		return "the setting " + name
	}
	return ""
}

// Has reports a module that is not core with that name on this node.
func (c *ModuleCatalog) Has(name string) bool {
	return c != nil && (slices.Contains(c.OfficialExtra, name) || slices.Contains(c.External, name))
}

// WhereItLooked says, for the error about a name that no module has, where
// MMWatchdog looked for the module.
func (c *ModuleCatalog) WhereItLooked() string {
	if c == nil {
		return "MMWatchdog has no core module of that name"
	}
	found := func(directory string, names []string) string {
		switch {
		case !primitives.IsDirectory(directory):
			return directory + ", which does not exist"
		case len(names) == 0:
			return directory + ", which holds no module"
		}
		return directory + " (" + strings.Join(names, ", ") + ")"
	}
	return "MMWatchdog has no core module of that name, and no directory of that name is in " + found(c.Dirs.OfficialExtra, c.OfficialExtra) + " or in " + found(c.Dirs.External, c.External)
}

// Module returns the module with that name as this node has it, with its
// description, or why MMWatchdog cannot use it.
func (c *ModuleCatalog) Module(name string) (*InstalledModule, error) {
	if !c.Has(name) {
		return nil, fmt.Errorf("no module is named %s: %s", name, c.WhereItLooked())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := c.read[name]; m != nil {
		return m, nil
	}
	if e := c.problems[name]; e != nil {
		return nil, e
	}
	m, e := c.readModule(name)
	if e != nil {
		c.problems[name] = e
		return nil, e
	}
	c.read[name] = m
	return m, nil
}

// readModule reads one module's directory: which kind of module it holds,
// and its description.
func (c *ModuleCatalog) readModule(name string) (*InstalledModule, error) {
	official := slices.Contains(c.OfficialExtra, name)
	m := &InstalledModule{Name: name, Kind: ModuleKindOfficialExtra, Directory: filepath.Join(c.Dirs.OfficialExtra, name)}
	if !official {
		m.Kind, m.Directory = ModuleKindProjectStarlark, filepath.Join(c.Dirs.External, name)
	}
	script, executable := fileExists(filepath.Join(m.Directory, ModuleScriptFile)), fileExists(filepath.Join(m.Directory, ModuleExecutableFile))
	switch {
	case official && (!script || executable):
		return nil, fmt.Errorf("the official extra module %s is not complete: %s must hold the script %s", name, m.Directory, ModuleScriptFile)
	case script && executable:
		return nil, fmt.Errorf("the module %s has both a script and an executable: %s must hold %s or %s, not both", name, m.Directory, ModuleScriptFile, ModuleExecutableFile)
	case !script && !executable:
		return nil, fmt.Errorf("the module %s has no code: %s must hold the script %s or the executable %s", name, m.Directory, ModuleScriptFile, ModuleExecutableFile)
	case executable:
		m.Kind = ModuleKindProjectExecutable
	}
	var e error
	if m.Description, e = ReadModuleDescription(m.Directory, name); e != nil {
		if errors.Is(e, os.ErrNotExist) {
			return nil, fmt.Errorf("the module %s has no description: %s must hold the file %s, which lists the module's inputs", name, m.Directory, ModuleDescriptionFile)
		}
		return nil, e
	}
	return m, nil
}

// fileExists reports a regular file at path.
func fileExists(path string) bool {
	info, e := os.Stat(path)
	return e == nil && info.Mode().IsRegular() //nolint:nilaway // os.Stat returns a nonnil FileInfo on success; the short-circuit error guard runs before its methods.
}

// Names lists every module that is not core on this node, in order of name.
func (c *ModuleCatalog) Names() []string {
	if c == nil {
		return nil
	}
	names := append(slices.Clone(c.OfficialExtra), c.External...)
	sort.Strings(names)
	return names
}

// Info returns what MMWatchdog knows about a module of the catalog.
func (c *ModuleCatalog) Info(name string) ModuleInfo {
	module, e := c.Module(name)
	if e != nil {
		return ModuleInfo{Name: name, Problem: e.Error()}
	}
	info := ModuleInfo{Name: name, Kind: module.Kind, Directory: module.Directory, Description: module.Description}
	if module.Kind == ModuleKindOfficialExtra {
		info.ShippedWithMMWatchdogVersion = Version
	}
	return info
}

// ModuleInfo is what MMWatchdog knows about one module.
type ModuleInfo struct {
	Name string `json:"name"`
	// Kind is core, official_extra, project_starlark, or project_executable.
	Kind string `json:"kind"`
	// Topic is, for a core module, the one of CoreModuleTopics that groups
	// it in the list of "mmwatchdog describe-module" and in the index of the module
	// pages.
	Topic string `json:"topic,omitempty"`
	// Directory holds the files of a module that is not core.
	Directory string `json:"directory,omitempty"`
	// ShippedWithMMWatchdogVersion is, for an official extra module, the version of
	// the MMWatchdog release that it belongs to.
	ShippedWithMMWatchdogVersion string `json:"shipped_with_mmwatchdog_version,omitempty"`
	// Description is the module's description. It is nil for a module that
	// MMWatchdog cannot use, and Problem then says why.
	Description *ModuleDescription `json:"description,omitempty"`
	Problem     string             `json:"problem,omitempty"`
}
