package checksandalerts_test

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

// FuzzModuleDescription reads raw bytes as the module_description.yml of a
// module that is not core, as MMWatchdog reads it before it runs the module, and
// checks what the parts of MMWatchdog that use a description rely on: the check of
// the inputs of a check or a destination, and "mmwatchdog describe-module". See
// docs/changing-mmwatchdog/testing.md, "Fuzz tests for a release".
func FuzzModuleDescription(f *testing.F) {
	type moduleAndDescription struct{ module, description string }
	// Each seed, with whether MMWatchdog accepts the description.
	acceptance := map[moduleAndDescription]bool{
		{"certificates", certificatesDescription}:          true,
		{"queued_jobs", testhelpers.QueuedJobsDescription}: true,
		{"own_alarm", testhelpers.TestModuleDescription("own_alarm", checksandalerts.UsedAsAlertDestination, map[string]string{"bell_name": "string", "rings": "integer"}, nil, "service_keeps_incidents: true")}: true,
		{"certificates", paddedWithAComment(certificatesDescription, checksandalerts.ModuleDescriptionLimit)}:                                                                                                     true,
		{"certificates", paddedWithAComment(certificatesDescription, checksandalerts.ModuleDescriptionLimit+1)}:                                                                                                   false,
		{"queued_jobs", certificatesDescription}: false,
		{"certificates", ""}:                     false,
		{"certificates", "summary: [unclosed\n"}: false,
		{"certificates", strings.Replace(certificatesDescription, "used_as: check", "used_as: alert_destination", 1)}:                     false,
		{"certificates", strings.Replace(certificatesDescription, "allowed_values: [7, 14, 30]", "allowed_values: [7, fourteen, 30]", 1)}: false,
	}
	// The descriptions of the official extra modules of the repository. A
	// compiled test program that runs without the source checkout finds
	// none.
	officialExtraDescriptions, err := filepath.Glob(filepath.Join("../../official_extra_modules", "*", checksandalerts.ModuleDescriptionFile))
	if err != nil {
		f.Fatal(err)
	}
	for _, file := range officialExtraDescriptions {
		text, err := os.ReadFile(file)
		if err != nil {
			f.Fatal(err)
		}
		acceptance[moduleAndDescription{filepath.Base(filepath.Dir(file)), string(text)}] = true
	}
	seeds := slices.Collect(maps.Keys(acceptance))
	slices.SortFunc(seeds, func(a, b moduleAndDescription) int {
		return strings.Compare(a.module+"/"+a.description, b.module+"/"+b.description)
	})
	for _, seed := range seeds {
		f.Add(seed.module, []byte(seed.description))
	}
	f.Fuzz(func(t *testing.T, module string, description []byte) {
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, checksandalerts.ModuleDescriptionFile), description, 0644); err != nil {
			t.Fatal(err)
		}
		first, firstErr := checksandalerts.ReadModuleDescription(directory, module)
		second, secondErr := checksandalerts.ReadModuleDescription(directory, module)
		if (firstErr == nil) != (secondErr == nil) || (firstErr == nil && !reflect.DeepEqual(first, second)) {
			t.Fatalf("two reads of the same file disagree: %+v, %v; %+v, %v", first, firstErr, second, secondErr)
		}
		if wantAccepted, isSeed := acceptance[moduleAndDescription{module, string(description)}]; isSeed && (firstErr == nil) != wantAccepted {
			t.Fatalf("a seed of the module %s: accepted %v, want %v: %v", module, firstErr == nil, wantAccepted, firstErr)
		}
		if tooLarge := len(description) > checksandalerts.ModuleDescriptionLimit; tooLarge != errors.Is(firstErr, primitives.ErrFileLargerThanLimit) {
			t.Fatalf("a description of %d bytes, with the limit %d: %v", len(description), checksandalerts.ModuleDescriptionLimit, firstErr)
		}
		if firstErr != nil {
			return
		}
		checkAcceptedModuleDescription(t, module, first)
	})
}

// checkAcceptedModuleDescription checks what MMWatchdog relies on in a description
// of the module named module that it accepted.
func checkAcceptedModuleDescription(t *testing.T, module string, d *checksandalerts.ModuleDescription) {
	t.Helper()
	// "mmwatchdog describe-module --json" writes the description.
	if _, err := json.Marshal(d); err != nil {
		t.Fatalf("the description cannot be written as JSON: %v", err)
	}
	if !isTrimmedText(d.Summary) {
		t.Fatalf("the summary is no text: %q", d.Summary)
	}
	if d.UsedAs != checksandalerts.UsedAsCheck && d.UsedAs != checksandalerts.UsedAsAlertDestination {
		t.Fatalf("used_as is %q", d.UsedAs)
	}
	if d.UsedAs != checksandalerts.UsedAsCheck && (len(d.Measurements) > 0 || d.OneIncidentForEachFinding) {
		t.Fatalf("a module used as %s reports measurements or findings", d.UsedAs)
	}
	if d.ServiceKeepsIncidents && d.UsedAs != checksandalerts.UsedAsAlertDestination {
		t.Fatalf("a module used as %s says that its service keeps incidents", d.UsedAs)
	}
	// The run settings stand beside the inputs of a module in a check or a
	// destination, so an input with the name of one could never be stated.
	runSettings := checksandalerts.Fields(reflect.TypeOf(primitives.ExecOptions{}))
	inputNames := map[string]bool{}
	for _, input := range d.Inputs {
		if !checksandalerts.ModuleNamePattern.MatchString(input.Name) || inputNames[input.Name] || runSettings[input.Name] != nil {
			t.Fatalf("the input %q cannot be stated, or the description lists it twice", input.Name)
		}
		inputNames[input.Name] = true
		if !slices.Contains(checksandalerts.ModuleInputTypes, input.Type) || !isTrimmedText(input.Description) {
			t.Fatalf("the input %s has the type %q and the description %q", input.Name, input.Type, input.Description)
		}
		if len(input.AllowedValues) > 0 && input.Type != "string" && input.Type != "integer" {
			t.Fatalf("the input %s of the type %s has allowed values", input.Name, input.Type)
		}
		// Each allowed value is one that a check or a destination can state.
		for _, allowed := range input.AllowedValues {
			if problem := d.InputsProblem(module, map[string]any{input.Name: allowed}); problem != nil && problem.Path == input.Name {
				t.Fatalf("the input %s refuses its allowed value %#v: %s", input.Name, allowed, problem.Message)
			}
		}
	}
	measurementNames := map[string]bool{}
	for _, measurement := range d.Measurements {
		if !checksandalerts.ModuleNamePattern.MatchString(measurement.Name) || measurementNames[measurement.Name] {
			t.Fatalf("the measurement %q is no name, or the description lists it twice", measurement.Name)
		}
		measurementNames[measurement.Name] = true
		if !isTrimmedText(measurement.Unit) || len(measurement.Unit) > 64 || !isTrimmedText(measurement.Description) {
			t.Fatalf("the measurement %s has the unit %q and the description %q", measurement.Name, measurement.Unit, measurement.Description)
		}
	}
	if len(d.Examples) == 0 {
		t.Fatal("the description has no example")
	}
	for i, example := range d.Examples {
		if !isTrimmedText(example.Description) || strings.TrimSpace(example.Configuration) == "" {
			t.Fatalf("example %d has the description %q and the configuration %q", i, example.Description, example.Configuration)
		}
		// Each example calls the module with inputs that the description
		// accepts, as a check or a destination states them.
		var configuration map[string]any
		if err := yaml.Unmarshal([]byte(example.Configuration), &configuration); err != nil {
			t.Fatalf("example %d is not YAML: %v", i, err)
		}
		stated, namesTheModule := configuration[module]
		values, isMapping := stated.(map[string]any)
		if !namesTheModule || (stated != nil && !isMapping) {
			t.Fatalf("example %d does not call the module %s with a mapping of its inputs: %q", i, module, example.Configuration)
		}
		inputs := map[string]any{}
		for name, value := range values {
			if runSettings[name] == nil {
				inputs[name] = value
			}
		}
		if problem := d.InputsProblem(module, inputs); problem != nil {
			t.Fatalf("example %d: %s.%s: %s", i, module, problem.Path, problem.Message)
		}
	}
}

// isTrimmedText reports a text that is not empty and has no space at its
// start or its end.
func isTrimmedText(text string) bool {
	return text != "" && strings.TrimSpace(text) == text
}

// paddedWithAComment returns text with a comment line at its end that makes
// it size bytes long.
func paddedWithAComment(text string, size int) string {
	return text + "#" + strings.Repeat("x", size-len(text)-2) + "\n"
}
