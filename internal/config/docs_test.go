package config_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/testhelpers"
)

// Every configuration block that the guides, the README, and the pages of the
// official extra modules and the design documents show is valid MMWatchdog
// configuration, so a reader can copy it. A block is a fenced yaml block
// whose top-level keys are check groups, alert destinations, or node
// settings; other yaml, such as MMX files, is skipped. Each is loaded with
// minimal companion files: valid node settings and one check group. A node
// settings block that shows only some settings gets the required node
// settings that it leaves out; check group and destination blocks must be complete as
// written. A block that names a module that is not core is checked against
// the modules of the repository (repositoryModuleDirs).
// namedDestinationsPattern finds destinations named by checks or silence settings.
var namedDestinationsPattern = regexp.MustCompile(`(?:send_alerts|send_silence_alerts_of_this_node)_only_to_destinations: \[([^\]]*)\]`)

// namedCausesPattern finds the checks that an example's checks name as
// their causes.
var namedCausesPattern = regexp.MustCompile(`no_alerts_or_recovery_actions_while_these_checks_fail: \[([^\]]*)\]`)

func TestDocumentedConfigurationIsValid(t *testing.T) {
	t.Parallel()
	testhelpers.NeedSourceCheckout(t)
	modules := repositoryModuleDirs(t)
	// Every page under docs/, except the generated module pages: their
	// sources, beside the code, are read below.
	var files []string
	filepath.WalkDir("../../docs", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path == filepath.Join("../../docs", "modules") {
			return fs.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	files = append(files, "../../README.md")
	pages, _ := filepath.Glob("../../official_extra_modules/*/README.md")
	files = append(files, pages...)
	// The page sources of the core modules, beside their code.
	sources, _ := filepath.Glob("../checksandalerts/*.md")
	files = append(files, sources...)
	fence := regexp.MustCompile("(?s)\n( *)```yaml\n(.*?)```")
	// Every setting that mmwatchdog_node_settings.yml takes.
	nodeSettings := checksandalerts.Fields(reflect.TypeOf(config.Config{}))
	// The settings that every node states, each with the value that a block
	// gets when it shows only some settings and leaves that one out.
	requiredSettings := map[string]string{"alert_severity_when_this_node_goes_silent": "sos", "alert_severity_when_this_node_has_rebooted": "sos",
		"keep_measurement_history_for": "7d", "measurement_history_size_limit": "256MiB", "keep_undelivered_alerts_for": "24h", "undelivered_alerts_content_size_limit": "8MiB"}
	const companionNode = "keep_undelivered_alerts_for: 24h\nundelivered_alerts_content_size_limit: 8MiB\nalert_severity_when_this_node_goes_silent: sos\nalert_severity_when_this_node_has_rebooted: warn\nkeep_measurement_history_for: 7d\nmeasurement_history_size_limit: 256MiB\nheartbeat_receiver: {listen_on_address: '127.0.0.1:8444', tls_certificate_file: /etc/mmwatchdog/secrets/cert.pem, tls_private_key_file: /etc/mmwatchdog/secrets/key.pem}\n"
	const companionGroups = "check_groups: {companion: {check_every: 30s, time_limit_per_run: 5s, alert_after_failing_for: 1m, recovered_after_healthy_for: 30s, shell: 'true', alert_severity: warn}}\n"
	checked := 0
	for _, file := range files {
		text, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range fence.FindAllStringSubmatchIndex(string(text), -1) {
			// A block indented in a list item is dedented by its fence's indent.
			indent := string(text[match[2]:match[3]])
			lines := strings.Split(string(text[match[4]:match[5]]), "\n")
			for i := range lines {
				lines[i] = strings.TrimPrefix(lines[i], indent)
			}
			block := strings.Join(lines, "\n")
			line := strings.Count(string(text[:match[4]]), "\n") + 1
			var top map[string]any
			if yaml.Unmarshal([]byte(block), &top) != nil || len(top) == 0 {
				continue
			}
			node, groups, alerts := companionNode, companionGroups, ""
			switch {
			case top["check_groups"] != nil:
				groups = block
			case top["destinations"] != nil:
				alerts = block
			default:
				settings := true
				for key := range top {
					settings = settings && nodeSettings[key] != nil
				}
				if !settings {
					continue
				}
				node = block
				for _, setting := range checksandalerts.Names(requiredSettings) {
					if top[setting] == nil {
						node += "\n" + setting + ": " + requiredSettings[setting] + "\n"
					}
				}
				if top["join_fleet_hub"] != nil || top["fleet_hub_role"] != nil {
					node += "\nheartbeat_receiver: {listen_on_address: '127.0.0.1:8444', tls_certificate_file: /etc/mmwatchdog/secrets/cert.pem, tls_private_key_file: /etc/mmwatchdog/secrets/key.pem}\n"
				}
			}
			dir := t.TempDir()
			os.MkdirAll(filepath.Join(dir, "check_groups"), 0700)
			os.WriteFile(filepath.Join(dir, "mmwatchdog_node_settings.yml"), []byte(node), 0600)
			os.WriteFile(filepath.Join(dir, "check_groups", "doc.yml"), []byte(groups), 0600)
			// A check that an example names as its cause can be in a check
			// group of another block of the same document. Such a check
			// group gets a stand-in with the named checks.
			if shown, ok := top["check_groups"].(map[string]any); ok {
				standIns := map[string][]string{}
				for _, list := range namedCausesPattern.FindAllStringSubmatch(groups, -1) {
					for _, name := range strings.Split(list[1], ",") {
						group, check, _ := strings.Cut(strings.TrimSpace(name), "/")
						if _, here := shown[group]; !here && check != "" {
							standIns[group] = append(standIns[group], check)
						}
					}
				}
				other := "check_groups:\n"
				for _, group := range checksandalerts.Names(standIns) {
					other += "  " + group + ": {check_every: 30s, time_limit_per_run: 5s, alert_after_failing_for: 1m, recovered_after_healthy_for: 30s, checks: {"
					for _, check := range standIns[group] {
						other += check + ": {shell: 'true', alert_severity: warn}, "
					}
					other += "}}\n"
				}
				if len(standIns) > 0 {
					os.WriteFile(filepath.Join(dir, "check_groups", "named_as_causes.yml"), []byte(other), 0600)
				}
			}
			switch {
			case alerts != "":
				os.WriteFile(filepath.Join(dir, "alert_destinations.yml"), []byte(alerts), 0600)
			case !strings.Contains(node, "join_fleet_hub"):
				// An example about another file runs with no destination,
				// except the destinations that its checks or silence settings name: the file
				// that defines them is another block of the same document.
				destinations := "destinations: {"
				for _, list := range namedDestinationsPattern.FindAllStringSubmatch(groups+"\n"+node, -1) {
					for _, name := range strings.Split(list[1], ",") {
						if name = strings.TrimSpace(name); name != "" && !strings.Contains(destinations, " "+name+":") {
							destinations += " " + name + ": {webhook: {url: 'https://example.com/'}, remind_every: never, alert_severities: []},"
						}
					}
				}
				os.WriteFile(filepath.Join(dir, "alert_destinations.yml"), []byte(destinations+"}\n"), 0600)
			}
			checked++
			if _, err := config.LoadWithModuleDirs(dir, modules); err != nil {
				t.Errorf("%s:%d: %v", file, line, err)
			}
		}
	}
	if checked < 20 {
		t.Fatal("found only", checked, "configuration blocks")
	}
}

// repositoryModuleDirs are the two module directories that the repository
// holds: the official extra modules of the release, and the example modules
// of a project's own, which the guides and the shipped examples name. A
// deploy installs such directories on every node.
func repositoryModuleDirs(t *testing.T) checksandalerts.ModuleDirs {
	t.Helper()
	dirs := checksandalerts.ModuleDirs{}
	for relative, directory := range map[string]*string{"../../official_extra_modules": &dirs.OfficialExtra, "../../examples/modules": &dirs.External} {
		absolute, err := filepath.Abs(relative)
		if err != nil {
			t.Fatal(err)
		}
		*directory = absolute
	}
	return dirs
}

// The shipped examples and deploy files load as complete configurations,
// with the modules of the repository.
func TestShippedExamplesAreValid(t *testing.T) {
	t.Parallel()
	testhelpers.NeedSourceCheckout(t)
	for _, dir := range []string{"../../examples", "../../examples/deploy/files/hub_and_standalone", "../../examples/deploy/files/join_hub",
		"../../examples/web-service-fleet/application_server", "../../examples/web-service-fleet/utility_host"} {
		if _, err := config.LoadWithModuleDirs(dir, repositoryModuleDirs(t)); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}
}

// Every example module of the repository has a description that MMWatchdog reads,
// and each example of a description is a valid check or a valid destination
// as it is written. An example module that is the source of an executable
// has no code until someone builds it; its description is checked all the
// same.
func TestShippedExampleModulesAreValid(t *testing.T) {
	t.Parallel()
	testhelpers.NeedSourceCheckout(t)
	dirs := repositoryModuleDirs(t)
	catalog, err := checksandalerts.LoadModuleCatalog(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.External) == 0 {
		t.Fatal("found no example module in", dirs.External)
	}
	for _, name := range catalog.External {
		directory := filepath.Join(dirs.External, name)
		description, err := checksandalerts.ReadModuleDescription(directory, name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if _, err := catalog.Module(name); err != nil {
			if sources, _ := filepath.Glob(filepath.Join(directory, "*.go")); len(sources) == 0 || !strings.Contains(err.Error(), "has no code") {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		for _, example := range description.Examples {
			if err := testhelpers.LoadExample(t, dirs, description.UsedAs, example); err != nil {
				t.Errorf("%s: the example %q is not valid: %v", name, example.Description, err)
			}
		}
	}
}
