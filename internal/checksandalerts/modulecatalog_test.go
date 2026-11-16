package checksandalerts_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/testhelpers"
)

func TestModulesAreNamedForWhatTheyLookAtAndNeverTakeABooleanWholeValue(t *testing.T) {
	t.Parallel()
	// A word of a claim, as a word of its own between underscores, such as
	// is in systemd_unit_is_active. installed is not one, because
	// known_exploited_vulnerabilities_in_installed_packages names packages.
	claimWords := []string{"is", "are", "has", "have", "runs", "running", "exists", "healthy", "synchronized"}
	check := func(name string, description *checksandalerts.ModuleDescription) {
		t.Helper()
		problem := ""
		for _, word := range strings.Split(name, "_") {
			if slices.Contains(claimWords, word) {
				problem = "its name holds the claim word " + word
			}
		}
		switch {
		case strings.HasPrefix(name, "no_"):
			problem = "its name starts with no_"
		case description.Value != nil && description.Value.Type == "boolean":
			problem = "it is called with true as its whole value"
		}
		if problem != "" {
			t.Errorf("module %s breaks the naming rule of docs/changing-mmwatchdog/design/checks.md (\"A module is named for what it looks at\"): %s. A module's name says what the check looks at, never a claim that should be true: it never starts with no_ and never holds %s as a word of its own, and a module is never called with true as its whole value. Name the thing that the check looks at, and say when the check fails with fail_when", name, problem, strings.Join(claimWords, ", "))
		}
	}
	for _, module := range checksandalerts.CoreCheckModules {
		check(module.Name, module.Description)
	}
	for _, module := range checksandalerts.CoreDestinationModules {
		check(module.Name, module.Description)
	}
	testhelpers.NeedSourceCheckout(t)
	official, err := filepath.Abs("../../official_extra_modules")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{OfficialExtra: official})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range catalog.OfficialExtra {
		module, err := catalog.Module(name)
		if err != nil {
			t.Error(err)
			continue
		}
		check(name, module.Description)
	}
}

// A node holds the modules that are not core in two directories. Each
// directory in one of them is one module, under the directory's name; a file
// is none. A directory that does not exist holds no module.
func TestTheCatalogListsTheModulesOfTheTwoDirectories(t *testing.T) {
	t.Parallel()
	official := testhelpers.OfficialExtraModules(t, checksandalerts.Version, "telegram", "slack")
	external := t.TempDir()
	testhelpers.InstallStarlarkModule(t, external, "queued_jobs", testhelpers.TestModuleDescription("queued_jobs", checksandalerts.UsedAsCheck, nil, nil), testhelpers.ScriptOfACheck)
	testhelpers.InstallModule(t, external, "legacy_backup", testhelpers.TestModuleDescription("legacy_backup", checksandalerts.UsedAsCheck, nil, nil), "true")
	for _, dir := range []string{official, external} {
		if e := os.WriteFile(filepath.Join(dir, "README.md"), []byte("How to set the modules up.\n"), 0644); e != nil {
			t.Fatal(e)
		}
	}
	catalog, e := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{OfficialExtra: official, External: external})
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(catalog.OfficialExtra, []string{"slack", "telegram"}) || !reflect.DeepEqual(catalog.External, []string{"legacy_backup", "queued_jobs"}) ||
		!reflect.DeepEqual(catalog.Names(), []string{"legacy_backup", "queued_jobs", "slack", "telegram"}) || catalog.Has("README.md") || catalog.Has("http") || !catalog.Has("slack") {
		t.Fatalf("%v %v", catalog.OfficialExtra, catalog.External)
	}
	for name, want := range map[string]checksandalerts.InstalledModule{
		"slack":         {Name: "slack", Kind: checksandalerts.ModuleKindOfficialExtra, Directory: filepath.Join(official, "slack")},
		"queued_jobs":   {Name: "queued_jobs", Kind: checksandalerts.ModuleKindProjectStarlark, Directory: filepath.Join(external, "queued_jobs")},
		"legacy_backup": {Name: "legacy_backup", Kind: checksandalerts.ModuleKindProjectExecutable, Directory: filepath.Join(external, "legacy_backup")},
	} {
		module, e := catalog.Module(name)
		if e != nil || module.Name != want.Name || module.Kind != want.Kind || module.Directory != want.Directory || module.Description == nil || module.IsStarlark() != (want.Kind != checksandalerts.ModuleKindProjectExecutable) {
			t.Errorf("%s: %+v %v", name, module, e)
		}
		if again, _ := catalog.Module(name); again != module {
			t.Errorf("%s was read a second time", name)
		}
	}
	// A name that no module has says where MMWatchdog looked.
	if _, e := catalog.Module("unavailable_alarm"); e == nil || e.Error() != "no module is named unavailable_alarm: MMWatchdog has no core module of that name, and no directory of that name is in "+official+" (slack, telegram) or in "+external+" (legacy_backup, queued_jobs)" {
		t.Fatal(e)
	}

	// A node that has neither directory runs, and so does one whose
	// directories hold no module.
	missing, empty := filepath.Join(t.TempDir(), "official_extra_modules"), t.TempDir()
	catalog, e = checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{OfficialExtra: missing, External: empty})
	if e != nil || len(catalog.Names()) != 0 || catalog.Has("slack") {
		t.Fatal(catalog, e)
	}
	if _, e := catalog.Module("slack"); e == nil || e.Error() != "no module is named slack: MMWatchdog has no core module of that name, and no directory of that name is in "+missing+", which does not exist or in "+empty+", which holds no module" {
		t.Fatal(e)
	}
	// A place that asks without a catalog has no module that is not core.
	var none *checksandalerts.ModuleCatalog
	if none.Has("slack") || len(none.Names()) != 0 || none.WhereItLooked() != "MMWatchdog has no core module of that name" {
		t.Fatal("a missing catalog has modules")
	}
	if _, installed := none.ModuleIfInstalled("slack"); installed {
		t.Fatal("a missing catalog has the module slack")
	}
}

// The official extra modules belong to one MMWatchdog release: the directory holds
// the version that they were shipped with, and MMWatchdog refuses a directory of
// another version, or without one, and names both versions.
func TestTheOfficialExtraModulesAreOfThisVersionOfMMWatchdog(t *testing.T) {
	if checksandalerts.Version != "0.1.0-dev" {
		t.Fatal("a test build of MMWatchdog has the version", checksandalerts.Version)
	}
	other := testhelpers.OfficialExtraModules(t, "1.0.0", "slack")
	if _, e := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{OfficialExtra: other}); e == nil || e.Error() != "the official extra modules in "+other+" were shipped with MMWatchdog 1.0.0, and this mmwatchdog program is MMWatchdog 0.1.0-dev. One MMWatchdog release holds both: deploy that release again" {
		t.Fatal(e)
	}
	// The check is of the directory, also when it holds no module yet and
	// when no check and no destination names one.
	for _, names := range [][]string{{"slack"}, {}} {
		unversioned := testhelpers.OfficialExtraModules(t, "", names...)
		if _, e := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{OfficialExtra: unversioned}); e == nil || e.Error() != unversioned+" has no file shipped_with_mmwatchdog_version, so it is not the official_extra_modules directory of an MMWatchdog release: deploy an MMWatchdog release again, which installs that directory beside the mmwatchdog program" {
			t.Fatal(names, e)
		}
	}
	// The same check stops a configuration from loading.
	directory := testhelpers.ConfigFile(t, "official_extra_module_dir: "+other+"\ncheck_groups: {a: {shell: 'true'}}")
	if _, e := config.Load(directory); e == nil || !strings.Contains(e.Error(), "were shipped with MMWatchdog 1.0.0, and this mmwatchdog program is MMWatchdog 0.1.0-dev") {
		t.Fatal(e)
	}
	current := testhelpers.OfficialExtraModules(t, checksandalerts.Version, "slack")
	if _, e := config.LoadWithModuleDirs(directory, checksandalerts.ModuleDirs{OfficialExtra: current}); e != nil {
		t.Fatal("the official extra modules of this version were refused:", e)
	}
	// The version of the mmwatchdog program is the one that the build gave it.
	defer func(version string) { checksandalerts.Version = version }(checksandalerts.Version)
	checksandalerts.Version = "1.0.0"
	if _, e := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{OfficialExtra: other}); e != nil {
		t.Fatal(e)
	}
	if _, e := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{OfficialExtra: current}); e == nil || !strings.Contains(e.Error(), "were shipped with MMWatchdog 0.1.0-dev, and this mmwatchdog program is MMWatchdog 1.0.0") {
		t.Fatal(e)
	}
}

// A name means one module on every node. A module of the project's own that
// has the name of a core module, of an official extra module, or of a
// setting is refused, and so is a directory whose name no module can have.
func TestTheCatalogRefusesANameThatIsTaken(t *testing.T) {
	t.Parallel()
	official := testhelpers.OfficialExtraModules(t, checksandalerts.Version, "slack")
	another := "A name means one module on every node: give your module another name"
	for name, taken := range map[string]string{
		"http":               "the core module http",
		"every_systemd_unit": "the core module every_systemd_unit",
		"email":              "the core module email",
		"mmhotline":          "the core module mmhotline",
		"slack":              "the official extra module slack, which every deploy installs",
		"checks":             "the setting checks",
		"alert_severity":     "the setting alert_severity",
		"alert_severities":   "the setting alert_severities",
		"heartbeat":          "the setting heartbeat",
		"check_every":        "the setting check_every",
		"remind_every":       "the setting remind_every",
	} {
		external := t.TempDir()
		testhelpers.InstallStarlarkModule(t, external, name, testhelpers.TestModuleDescription(name, checksandalerts.UsedAsCheck, nil, nil), testhelpers.ScriptOfACheck)
		if _, e := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{OfficialExtra: official, External: external}); e == nil || e.Error() != "the module "+name+" in "+external+" has the name of "+taken+". "+another {
			t.Errorf("%s: %v", name, e)
		}
	}
	// The name of an official extra module is free where the node has none.
	external := t.TempDir()
	testhelpers.InstallStarlarkModule(t, external, "slack", testhelpers.TestModuleDescription("slack", checksandalerts.UsedAsAlertDestination, nil, nil), testhelpers.ScriptOfADestination)
	if _, e := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{External: external}); e != nil {
		t.Fatal(e)
	}
	// An official extra module never has the name of a core module either.
	clash := testhelpers.OfficialExtraModules(t, checksandalerts.Version, "webhook")
	if _, e := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{OfficialExtra: clash}); e == nil || e.Error() != "the official extra module webhook in "+clash+" has the name of the core module webhook" {
		t.Fatal(e)
	}
	noName := "the name of a module uses lower-case letters, digits, and underscores, starts with a letter, and has at most 64 characters, such as queued_jobs"
	for _, name := range []string{"own-alarm", "Queue", "2fa", "_private", "queue.d", strings.Repeat("q", 65)} {
		external, release := t.TempDir(), testhelpers.OfficialExtraModules(t, checksandalerts.Version)
		for dir, dirs := range map[string]checksandalerts.ModuleDirs{external: {External: external}, release: {OfficialExtra: release}} {
			if e := os.Mkdir(filepath.Join(dir, name), 0755); e != nil {
				t.Fatal(e)
			}
			if _, e := checksandalerts.LoadModuleCatalog(dirs); e == nil || e.Error() != filepath.Join(dir, name)+" is no module: "+noName {
				t.Errorf("%s: %v", name, e)
			}
		}
	}
	for _, name := range []string{"q", "queued_jobs_2", strings.Repeat("q", 64)} {
		external := t.TempDir()
		if e := os.Mkdir(filepath.Join(external, name), 0755); e != nil {
			t.Fatal(e)
		}
		if catalog, e := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{External: external}); e != nil || !catalog.Has(name) {
			t.Errorf("%s: %v", name, e)
		}
	}
}

// MMWatchdog loads only the modules that the node's configuration names. A module
// whose directory is not complete does not stop a node that does not use it,
// and is refused with the reason when a check or a destination names it: a
// module without code, with both a script and an executable, or without its
// description, and an official extra module that is no script.
func TestAModuleThatIsNotCompleteIsRefusedWhenItIsNamed(t *testing.T) {
	t.Parallel()
	official := testhelpers.OfficialExtraModules(t, checksandalerts.Version, "slack")
	external := t.TempDir()
	write := func(dir, module, file, content string, mode os.FileMode) {
		t.Helper()
		if e := os.MkdirAll(filepath.Join(dir, module), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(dir, module, file), []byte(content), mode); e != nil {
			t.Fatal(e)
		}
	}
	// no_code holds only its description.
	write(external, "no_code", checksandalerts.ModuleDescriptionFile, testhelpers.TestModuleDescription("no_code", checksandalerts.UsedAsCheck, nil, nil), 0644)
	testhelpers.InstallStarlarkModule(t, external, "both", testhelpers.TestModuleDescription("both", checksandalerts.UsedAsCheck, nil, nil), testhelpers.ScriptOfACheck)
	write(external, "both", checksandalerts.ModuleExecutableFile, "#!/bin/bash\n", 0755)
	write(external, "no_description", checksandalerts.ModuleScriptFile, testhelpers.ScriptOfACheck, 0644)
	write(external, "wrong_description", checksandalerts.ModuleScriptFile, testhelpers.ScriptOfACheck, 0644)
	write(external, "wrong_description", checksandalerts.ModuleDescriptionFile, "summary: A module of a test.\n", 0644)
	// A directory instead of the script is no script.
	write(external, "no_file", checksandalerts.ModuleDescriptionFile, testhelpers.TestModuleDescription("no_file", checksandalerts.UsedAsCheck, nil, nil), 0644)
	if e := os.Mkdir(filepath.Join(external, "no_file", checksandalerts.ModuleScriptFile), 0755); e != nil {
		t.Fatal(e)
	}
	testhelpers.InstallModule(t, official, "slack", testhelpers.TestModuleDescription("slack", checksandalerts.UsedAsAlertDestination, nil, nil), "true")
	write(official, "telegram", checksandalerts.ModuleDescriptionFile, testhelpers.TestModuleDescription("telegram", checksandalerts.UsedAsAlertDestination, nil, nil), 0644)
	write(official, "pushover", checksandalerts.ModuleExecutableFile, "#!/bin/bash\n", 0755)
	testhelpers.InstallStarlarkModule(t, official, "pushover", testhelpers.TestModuleDescription("pushover", checksandalerts.UsedAsAlertDestination, nil, nil), testhelpers.ScriptOfADestination)
	testhelpers.InstallStarlarkModule(t, external, "queued_jobs", testhelpers.TestModuleDescription("queued_jobs", checksandalerts.UsedAsCheck, nil, nil), testhelpers.ScriptOfACheck)

	catalog, e := checksandalerts.LoadModuleCatalog(checksandalerts.ModuleDirs{OfficialExtra: official, External: external})
	if e != nil {
		t.Fatal("a module that nothing names stopped MMWatchdog:", e)
	}
	problems := map[string]string{
		"no_code":           "the module no_code has no code: " + filepath.Join(external, "no_code") + " must hold the script module.star or the executable module_executable",
		"no_file":           "the module no_file has no code: " + filepath.Join(external, "no_file") + " must hold the script module.star or the executable module_executable",
		"both":              "the module both has both a script and an executable: " + filepath.Join(external, "both") + " must hold module.star or module_executable, not both",
		"no_description":    "the module no_description has no description: " + filepath.Join(external, "no_description") + " must hold the file module_description.yml, which lists the module's inputs",
		"wrong_description": filepath.Join(external, "wrong_description", checksandalerts.ModuleDescriptionFile) + ":1: the description: used_as is required",
		"slack":             "the official extra module slack is not complete: " + filepath.Join(official, "slack") + " must hold the script module.star",
		"telegram":          "the official extra module telegram is not complete: " + filepath.Join(official, "telegram") + " must hold the script module.star",
		"pushover":          "the official extra module pushover is not complete: " + filepath.Join(official, "pushover") + " must hold the script module.star",
	}
	for name, want := range problems {
		for range 2 {
			if _, e := catalog.Module(name); e == nil || e.Error() != want {
				t.Errorf("%s:\n got %v\nwant %s", name, e, want)
			}
		}
		if _, installed := catalog.ModuleIfInstalled(name); installed {
			t.Errorf("%s counts as installed", name)
		}
	}
	// A configuration that names none of them loads, and one that names
	// one is refused at the place that names it.
	directory := testhelpers.ConfigFile(t, "check_groups:\n  jobs:\n    queued_jobs: {}\n")
	if _, e := config.LoadWithModuleDirs(directory, checksandalerts.ModuleDirs{OfficialExtra: official, External: external}); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"no_code", "both", "no_description", "wrong_description"} {
		directory := testhelpers.ConfigFile(t, "check_groups:\n  jobs:\n    "+name+": {}\n")
		if _, e := config.LoadWithModuleDirs(directory, checksandalerts.ModuleDirs{OfficialExtra: official, External: external}); e == nil || !strings.Contains(e.Error(), filepath.Join(directory, "check_groups", "checks.yml")+":3:") || !strings.HasSuffix(e.Error(), "check_groups.jobs."+name+": "+problems[name]) {
			t.Errorf("%s:\n got %v\nwant %s", name, e, problems[name])
		}
	}
	alerts := filepath.Join(directory, "alert_destinations.yml")
	if e := os.WriteFile(alerts, []byte("destinations:\n  on_call:\n    remind_every: never\n    slack: {}\n    alert_severities: [sos]\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := config.LoadWithModuleDirs(directory, checksandalerts.ModuleDirs{OfficialExtra: official, External: external}); e == nil || !strings.Contains(e.Error(), alerts+":4:") || !strings.HasSuffix(e.Error(), "destinations.on_call.slack: "+problems["slack"]) {
		t.Fatal(e)
	}
}
