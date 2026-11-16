package config

import (
	"path/filepath"
	"sort"

	"go.yaml.in/yaml/v3"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/primitives"
)

// Every check and every destination calls its module by the module's name
// alone, so two commands show what the configuration does not: "mmwatchdog
// describe-module" says what one module is and what its inputs are, and "mmwatchdog
// module-usage" lists the modules that this node's configuration names,
// with the kind of each and every place that calls it. The rules are in
// docs/changing-mmwatchdog/design/checks.md under "The module description" and "Which modules a
// node uses".

// ModuleUse is one module that a configuration names, with every place
// that calls it.
type ModuleUse struct {
	checksandalerts.ModuleInfo
	// CalledAt are the places where a check or a destination names the
	// module, in order of file and line.
	CalledAt []checksandalerts.SourceLocation `json:"called_at"`
}

// NodeModuleDirs returns the two module directories that the node settings
// in a configuration directory state, or MMWatchdog's own for a setting that the
// file does not state, and for a file that cannot be read.
func NodeModuleDirs(configDir string) checksandalerts.ModuleDirs {
	dirs := checksandalerts.ModuleDirs{OfficialExtra: checksandalerts.DefaultOfficialExtraModuleDir, External: checksandalerts.DefaultExternalModuleDir}
	b, e := primitives.ReadFile(filepath.Join(configDir, "mmwatchdog_node_settings.yml"), 1024*1024)
	if e != nil {
		return dirs
	}
	var stated struct {
		OfficialExtraModuleDir string `yaml:"official_extra_module_dir"`
		ExternalModuleDir      string `yaml:"external_module_dir"`
	}
	if yaml.Unmarshal(b, &stated) != nil {
		return dirs
	}
	if filepath.IsAbs(stated.OfficialExtraModuleDir) {
		dirs.OfficialExtra = stated.OfficialExtraModuleDir
	}
	if filepath.IsAbs(stated.ExternalModuleDir) {
		dirs.External = stated.ExternalModuleDir
	}
	return dirs
}

// coreModuleInfo returns the core module with that name.
func coreModuleInfo(name string) (checksandalerts.ModuleInfo, bool) {
	if m := checksandalerts.CheckModulesByName[name]; m != nil {
		return checksandalerts.ModuleInfo{Name: name, Kind: checksandalerts.ModuleKindCore, Topic: m.Topic, Description: m.Description}, true
	}
	if m := checksandalerts.DestinationModulesByName[name]; m != nil {
		return checksandalerts.ModuleInfo{Name: name, Kind: checksandalerts.ModuleKindCore, Topic: m.Topic, Description: m.Description}, true
	}
	return checksandalerts.ModuleInfo{}, false
}

// CoreModule is what the module page of a core module is made from, beside
// the module's page source: the generator in docs/reference/modules reads it.
type CoreModule struct {
	Name string
	// UsedAs is check or alert_destination.
	UsedAs string
	// Topic is one of CoreModuleTopics.
	Topic       string
	Description *checksandalerts.ModuleDescription
	// CodeFile is the file in internal/checksandalerts that holds the module's code and
	// its description: check_NAME.go or destination_NAME.go.
	CodeFile string
	// The fields of a check module's registry line that say what a check
	// does with the module's measurements. module.go says what each means.
	Measures, FailsWithoutALimit, LimitsOnItsMeasurements bool
}

// CoreModules returns the core modules: the check modules, then the
// destination modules, each in the order of its registry.
func CoreModules() []CoreModule {
	modules := []CoreModule{}
	for _, m := range checksandalerts.CoreCheckModules {
		modules = append(modules, CoreModule{Name: m.Name, UsedAs: checksandalerts.UsedAsCheck, Topic: m.Topic, Description: m.Description, CodeFile: "check_" + m.Name + ".go",
			Measures: m.Measures, FailsWithoutALimit: m.FailsWithoutALimit, LimitsOnItsMeasurements: m.LimitsOnItsMeasurements})
	}
	for _, m := range checksandalerts.CoreDestinationModules {
		modules = append(modules, CoreModule{Name: m.Name, UsedAs: checksandalerts.UsedAsAlertDestination, Topic: m.Topic, Description: m.Description, CodeFile: "destination_" + m.Name + ".go"})
	}
	return modules
}

// DescribeModule returns one module by its name: a core module, or a module
// of one of the two directories.
func DescribeModule(dirs checksandalerts.ModuleDirs, name string) (checksandalerts.ModuleInfo, error) {
	if info, core := coreModuleInfo(name); core {
		return info, nil
	}
	catalog, e := checksandalerts.LoadModuleCatalog(dirs)
	if e != nil {
		return checksandalerts.ModuleInfo{}, e
	}
	if _, e := catalog.Module(name); e != nil {
		return checksandalerts.ModuleInfo{}, e
	}
	return catalog.Info(name), nil
}

// ListModules returns every module that a node has: the core modules in the
// order of the registries, checks first, then the modules of the two
// directories in order of name. A module that MMWatchdog cannot use is listed with
// its problem.
func ListModules(dirs checksandalerts.ModuleDirs) ([]checksandalerts.ModuleInfo, error) {
	modules := []checksandalerts.ModuleInfo{}
	for _, m := range checksandalerts.CoreCheckModules {
		modules = append(modules, checksandalerts.ModuleInfo{Name: m.Name, Kind: checksandalerts.ModuleKindCore, Topic: m.Topic, Description: m.Description})
	}
	for _, m := range checksandalerts.CoreDestinationModules {
		modules = append(modules, checksandalerts.ModuleInfo{Name: m.Name, Kind: checksandalerts.ModuleKindCore, Topic: m.Topic, Description: m.Description})
	}
	catalog, e := checksandalerts.LoadModuleCatalog(dirs)
	if e != nil {
		return nil, e
	}
	for _, name := range catalog.OfficialExtra {
		modules = append(modules, catalog.Info(name))
	}
	for _, name := range catalog.External {
		info := catalog.Info(name)
		if info.Kind == "" {
			info.Directory = filepath.Join(dirs.External, name)
		}
		modules = append(modules, info)
	}
	return modules, nil
}

// ModuleUsage lists the modules that the configuration names: for each
// module what MMWatchdog knows about it and every place that calls it. The order is
// the kinds as "mmwatchdog module-usage" prints them, and the name within a kind.
func (c *Config) ModuleUsage() []ModuleUse {
	uses := map[string]*ModuleUse{}
	called := func(name string, info checksandalerts.ModuleInfo, at checksandalerts.SourceLocation) {
		if uses[name] == nil {
			uses[name] = &ModuleUse{ModuleInfo: info}
		}
		uses[name].CalledAt = append(uses[name].CalledAt, at)
	}
	info := func(name string) checksandalerts.ModuleInfo {
		if core, isCore := coreModuleInfo(name); isCore {
			return core
		}
		return c.Modules.Info(name)
	}
	for _, group := range checksandalerts.Names(c.CheckGroups) {
		for _, name := range checksandalerts.Names(c.CheckGroups[group].Checks) {
			module := c.CheckGroups[group].Checks[name].ModuleName()
			// A check that is written directly on its check group has its
			// module there; a named check has it below its own name.
			at, direct := c.Sources["check_groups."+group+"."+module]
			if named, isNamed := c.Sources["check_groups."+group+".checks."+name+"."+module]; isNamed || !direct {
				at = named
			}
			called(module, info(module), at)
		}
	}
	for _, name := range checksandalerts.Names(c.Alerts.Destinations) {
		module := c.Alerts.Destinations[name].ModuleName()
		called(module, info(module), c.Sources["destinations."+name+"."+module])
	}
	order := map[string]int{checksandalerts.ModuleKindCore: 0, checksandalerts.ModuleKindOfficialExtra: 1, checksandalerts.ModuleKindProjectStarlark: 2, checksandalerts.ModuleKindProjectExecutable: 2}
	list := []ModuleUse{}
	for _, name := range checksandalerts.Names(uses) {
		use := *uses[name] //nolint:nilaway // called allocates every entry in uses; Names returns keys from that unchanged local map.
		sort.SliceStable(use.CalledAt, func(i, j int) bool {
			a, b := use.CalledAt[i], use.CalledAt[j]
			return a.File < b.File || (a.File == b.File && a.Line < b.Line)
		})
		list = append(list, use)
	}
	sort.SliceStable(list, func(i, j int) bool { return order[list[i].Kind] < order[list[j].Kind] })
	return list
}

// ModuleKindWords writes the kind of a module for a person, such as "the
// project's own, Starlark".
func ModuleKindWords(kind string) string {
	switch kind {
	case checksandalerts.ModuleKindCore:
		return "core"
	case checksandalerts.ModuleKindOfficialExtra:
		return "official extra"
	case checksandalerts.ModuleKindProjectStarlark:
		return "the project's own, Starlark"
	case checksandalerts.ModuleKindProjectExecutable:
		return "the project's own, executable"
	}
	return "not installed on this node"
}
