package checksandalerts

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"mmwatchdog/internal/primitives"
)

// Every module has a module description: its inputs, its measurements, and
// examples. A core module's description is built in, beside the module's
// code; every other module has the file module_description.yml beside its
// script or its executable. MMWatchdog checks every check and every destination
// against the description of its module before anything runs, and
// "mmwatchdog describe-module" prints it. The rules are in docs/changing-mmwatchdog/design/checks.md
// under "The module description".

// The two ways in which a module is used.
const (
	UsedAsCheck            = "check"
	UsedAsAlertDestination = "alert_destination"
)

// ModuleDescriptionFile is the name of a module's description file, and
// ModuleDescriptionLimit the most bytes that it holds.
const (
	ModuleDescriptionFile  = "module_description.yml"
	ModuleDescriptionLimit = 256 * 1024
)

// ModuleDescription says what a module does and what a check or a
// destination can state for it.
type ModuleDescription struct {
	Summary string `json:"summary"`
	// UsedAs is check or alert_destination.
	UsedAs string `json:"used_as"`
	// Value describes, for a core module, the one scalar that the module
	// takes as its value, such as the address of tcp, or that it takes in
	// place of its mapping of inputs, such as the script of shell.
	Value        *ModuleInput        `json:"value,omitempty"`
	Inputs       []ModuleInput       `json:"inputs,omitempty"`
	Measurements []ModuleMeasurement `json:"measurements,omitempty"`
	// OneIncidentForEachFinding marks a check module that reports findings.
	OneIncidentForEachFinding bool `json:"one_incident_for_each_finding,omitempty"`
	// ServiceKeepsIncidents marks a destination module whose service opens
	// an alert of its own for a failure and closes it for the recovery,
	// such as Pushover: MMWatchdog sends such a destination one closing alert when
	// the last node of a check's fleet incident recovers.
	ServiceKeepsIncidents bool            `json:"service_keeps_incidents,omitempty"`
	Examples              []ModuleExample `json:"examples"`
}

// ModuleInput is one input of a module.
type ModuleInput struct {
	Name string `json:"name,omitempty"`
	// Type is one of ModuleInputTypes, or for a core module one of
	// CoreModuleInputTypes too.
	Type     string `json:"type"`
	Required bool   `json:"required"`
	// AllowedValues are the values that the input may have, when it may not
	// have every value of its type.
	AllowedValues []any  `json:"allowed_values,omitempty"`
	Description   string `json:"description"`
}

// ModuleMeasurement is one measurement that a check module reports.
type ModuleMeasurement struct {
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Description string `json:"description"`
}

// ModuleExample is one example of a check or a destination that uses the
// module: what it shows, and its configuration as YAML.
type ModuleExample struct {
	Description   string `json:"description"`
	Configuration string `json:"config"`
}

// ModuleInputTypes are the types that an input in a description file can
// have. CoreModuleInputTypes are the further types of a core module's
// inputs, whose values the module's own code checks.
var (
	ModuleInputTypes     = []string{"string", "integer", "number", "boolean", "duration", "list_of_strings"}
	CoreModuleInputTypes = []string{"list_of_integers", "mapping_of_strings"}
)

// ModuleNamePattern accepts the name of a module and of an input: lower-case
// letters, digits, and underscores, starting with a letter, at most 64
// characters.
var ModuleNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// runSettings are the settings that say how MMWatchdog runs a module that is not
// core. They are written beside the module's inputs, so no input can have
// one of their names.
var runSettings = []string{"run_as_user_account", "run_from_directory", "env"}

// input returns the description of the input with that name.
func (d *ModuleDescription) input(name string) (ModuleInput, bool) {
	for _, input := range d.Inputs {
		if input.Name == name {
			return input, true
		}
	}
	return ModuleInput{}, false
}

// InputNames lists the names of the inputs, in the order of the description.
func (d *ModuleDescription) InputNames() []string {
	names := []string{}
	for _, input := range d.Inputs {
		names = append(names, input.Name)
	}
	return names
}

// MeasurementNames lists the names of the measurements, in the order of the
// description.
func (d *ModuleDescription) MeasurementNames() []string {
	names := []string{}
	for _, measurement := range d.Measurements {
		names = append(names, measurement.Name)
	}
	return names
}

// UnitOf returns the unit of the measurement with that name.
func (d *ModuleDescription) UnitOf(measurement string) (string, bool) {
	for _, m := range d.Measurements {
		if m.Name == measurement {
			return m.Unit, true
		}
	}
	return "", false
}

// ReadModuleDescription reads the description file of the module named
// module in directory and checks it. Every error names the file, and the
// line where the file has one.
func ReadModuleDescription(directory, module string) (*ModuleDescription, error) {
	path := directory + "/" + ModuleDescriptionFile
	b, e := primitives.ReadFile(path, ModuleDescriptionLimit)
	if e != nil {
		return nil, e
	}
	var document yaml.Node
	if e := yaml.Unmarshal(b, &document); e != nil {
		return nil, fmt.Errorf("%s: %w", path, e)
	}
	if len(document.Content) != 1 {
		return nil, fmt.Errorf("%s: expected a mapping", path)
	}
	d, e := parseModuleDescription(document.Content[0])
	if e == nil {
		e = d.checkExamples(module)
	}
	if e != nil {
		return nil, fmt.Errorf("%s:%w", path, e)
	}
	return d, nil
}

// descriptionError is an error of a description file at one of its lines.
func descriptionError(n *yaml.Node, format string, values ...any) error {
	return fmt.Errorf("%d: %s", n.Line, fmt.Sprintf(format, values...)) //nolint:nilaway // descriptionEntries builds names and their YAML nodes together; these lookups use those names, and YAML null is a nonnil Node. descriptionText already accepted this required field; it returns an error when the map has no node.
}

// descriptionEntries returns the entries of a mapping of a description file
// in the order written. It refuses a node that is no mapping, a key that is
// written twice, and a key that is not among known.
func descriptionEntries(n *yaml.Node, what string, known ...string) ([]string, map[string]*yaml.Node, error) {
	if n.Kind != yaml.MappingNode {
		return nil, nil, descriptionError(n, "%s: expected a mapping", what)
	}
	keys, values := []string{}, map[string]*yaml.Node{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if key.Tag != "!!str" || values[key.Value] != nil {
			return nil, nil, descriptionError(key, "%s: expected each key once, as a plain string: %q", what, key.Value)
		}
		if len(known) > 0 && !slices.Contains(known, key.Value) {
			return nil, nil, descriptionError(key, "%s: unknown field %q; the fields are %s", what, key.Value, strings.Join(known, ", "))
		}
		keys, values[key.Value] = append(keys, key.Value), value
	}
	return keys, values, nil
}

// descriptionText returns the text at a required field of a description
// file: one string that is not empty.
func descriptionText(parent *yaml.Node, values map[string]*yaml.Node, what, field string) (string, error) {
	n := values[field]
	if n == nil {
		return "", descriptionError(parent, "%s: %s is required", what, field)
	}
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" || strings.TrimSpace(n.Value) == "" {
		return "", descriptionError(n, "%s: %s: expected a text", what, field)
	}
	return strings.TrimSpace(n.Value), nil
}

// parseModuleDescription reads a description from the mapping of its file.
func parseModuleDescription(n *yaml.Node) (*ModuleDescription, error) {
	_, top, e := descriptionEntries(n, "the description", "summary", "used_as", "inputs", "measurements", "one_incident_for_each_finding", "service_keeps_incidents", "examples")
	if e != nil {
		return nil, e
	}
	d := &ModuleDescription{}
	if d.Summary, e = descriptionText(n, top, "the description", "summary"); e != nil {
		return nil, e
	}
	if d.UsedAs, e = descriptionText(n, top, "the description", "used_as"); e != nil {
		return nil, e
	}
	if d.UsedAs != UsedAsCheck && d.UsedAs != UsedAsAlertDestination {
		return nil, descriptionError(top["used_as"], "used_as: expected %s or %s", UsedAsCheck, UsedAsAlertDestination)
	}
	if inputs := top["inputs"]; inputs != nil {
		names, entries, e := descriptionEntries(inputs, "inputs")
		if e != nil {
			return nil, e
		}
		for _, name := range names {
			input, e := parseModuleInput(name, entries[name])
			if e != nil {
				return nil, e
			}
			d.Inputs = append(d.Inputs, input)
		}
	}
	if measurements := top["measurements"]; measurements != nil {
		if d.UsedAs != UsedAsCheck {
			return nil, descriptionError(measurements, "measurements: only a module that is used as a check reports measurements")
		}
		names, entries, e := descriptionEntries(measurements, "measurements")
		if e != nil {
			return nil, e
		}
		for _, name := range names {
			what := "measurements." + name
			if !ModuleNamePattern.MatchString(name) {
				return nil, descriptionError(entries[name], "%s: the name of a measurement uses lower-case letters, digits, and underscores, and starts with a letter", what)
			}
			_, fields, e := descriptionEntries(entries[name], what, "unit", "description")
			if e != nil {
				return nil, e
			}
			m := ModuleMeasurement{Name: name}
			if m.Unit, e = descriptionText(entries[name], fields, what, "unit"); e != nil {
				return nil, e
			}
			if len(m.Unit) > 64 {
				return nil, descriptionError(fields["unit"], "%s: unit: expected at most 64 bytes", what)
			}
			if m.Description, e = descriptionText(entries[name], fields, what, "description"); e != nil {
				return nil, e
			}
			d.Measurements = append(d.Measurements, m)
		}
	}
	if findings := top["one_incident_for_each_finding"]; findings != nil {
		if findings.Kind != yaml.ScalarNode || findings.Tag != "!!bool" || (findings.Value != "true" && findings.Value != "false") {
			return nil, descriptionError(findings, "one_incident_for_each_finding: expected true or false")
		}
		d.OneIncidentForEachFinding = findings.Value == "true"
		if d.OneIncidentForEachFinding && d.UsedAs != UsedAsCheck {
			return nil, descriptionError(findings, "one_incident_for_each_finding: only a module that is used as a check reports findings")
		}
	}
	if keeps := top["service_keeps_incidents"]; keeps != nil {
		if keeps.Kind != yaml.ScalarNode || keeps.Tag != "!!bool" || (keeps.Value != "true" && keeps.Value != "false") {
			return nil, descriptionError(keeps, "service_keeps_incidents: expected true or false")
		}
		d.ServiceKeepsIncidents = keeps.Value == "true"
		if d.ServiceKeepsIncidents && d.UsedAs != UsedAsAlertDestination {
			return nil, descriptionError(keeps, "service_keeps_incidents: only a module that is used as an alert destination delivers to a service")
		}
	}
	examples := top["examples"]
	if examples == nil || examples.Kind != yaml.SequenceNode || len(examples.Content) == 0 {
		return nil, descriptionError(n, "examples: is required: a list of at least one example, each with a description and a config")
	}
	for i, example := range examples.Content {
		what := fmt.Sprintf("examples.%d", i)
		_, fields, e := descriptionEntries(example, what, "description", "config")
		if e != nil {
			return nil, e
		}
		parsed := ModuleExample{}
		if parsed.Description, e = descriptionText(example, fields, what, "description"); e != nil {
			return nil, e
		}
		if _, e = descriptionText(example, fields, what, "config"); e != nil {
			return nil, e
		}
		parsed.Configuration = fields["config"].Value //nolint:nilaway // descriptionText already accepted this required field; it returns an error when the map has no node.
		d.Examples = append(d.Examples, parsed)
	}
	return d, nil
}

// parseModuleInput reads the description of one input.
func parseModuleInput(name string, n *yaml.Node) (ModuleInput, error) {
	what := "inputs." + name
	input := ModuleInput{Name: name}
	if !ModuleNamePattern.MatchString(name) {
		return input, descriptionError(n, "%s: the name of an input uses lower-case letters, digits, and underscores, and starts with a letter", what)
	}
	if slices.Contains(runSettings, name) {
		return input, descriptionError(n, "%s: no input can have this name: %s says how MMWatchdog runs every module that is not core", what, name)
	}
	_, fields, e := descriptionEntries(n, what, "type", "required", "allowed_values", "description")
	if e != nil {
		return input, e
	}
	if input.Type, e = descriptionText(n, fields, what, "type"); e != nil {
		return input, e
	}
	if !slices.Contains(ModuleInputTypes, input.Type) {
		return input, descriptionError(fields["type"], "%s: type: expected one of %s", what, strings.Join(ModuleInputTypes, ", "))
	}
	required := fields["required"]
	if required == nil {
		return input, descriptionError(n, "%s: set required to true for a required input or false for an optional input", what)
	}
	if required.Kind != yaml.ScalarNode || required.Tag != "!!bool" || (required.Value != "true" && required.Value != "false") {
		return input, descriptionError(required, "%s: required: expected true or false", what)
	}
	input.Required = required.Value == "true"
	if input.Description, e = descriptionText(n, fields, what, "description"); e != nil {
		return input, e
	}
	if allowed := fields["allowed_values"]; allowed != nil {
		if input.Type != "string" && input.Type != "integer" {
			return input, descriptionError(allowed, "%s: allowed_values: only an input of the type string or integer takes a list of allowed values", what)
		}
		var values []any
		if allowed.Kind != yaml.SequenceNode || allowed.Decode(&values) != nil || len(values) == 0 {
			return input, descriptionError(allowed, "%s: allowed_values: expected a list of at least one value", what)
		}
		for _, value := range values {
			if problem := inputTypeProblem(input.Type, value); problem != "" {
				return input, descriptionError(allowed, "%s: allowed_values: %s", what, problem)
			}
		}
		input.AllowedValues = values
	}
	return input, nil
}

// checkExamples checks each example of a description file against the
// description's own inputs: the example's configuration is a mapping that
// names the module, and what it states for the module is valid.
func (d *ModuleDescription) checkExamples(module string) error {
	for i, example := range d.Examples {
		what := fmt.Sprintf("examples.%d.config", i)
		var configuration map[string]any
		if e := yaml.Unmarshal([]byte(example.Configuration), &configuration); e != nil || configuration == nil {
			return fmt.Errorf(" %s: expected the YAML of one check or one destination that names the module %s", what, module)
		}
		stated, names := configuration[module]
		if !names {
			return fmt.Errorf(" %s: the example does not name the module %s", what, module)
		}
		values, _ := stated.(map[string]any)
		if stated != nil && values == nil {
			return fmt.Errorf(" %s: %s: expected a mapping of the module's inputs", what, module)
		}
		inputs := map[string]any{}
		for name, value := range values {
			if !slices.Contains(runSettings, name) {
				inputs[name] = value
			}
		}
		if problem := d.InputsProblem(module, inputs); problem != nil {
			return fmt.Errorf(" %s: %s.%s: %s", what, module, problem.Path, problem.Message)
		}
	}
	return nil
}

// InputsProblem checks what a check or a destination states for the module
// against the description: every input is one that the module has, every
// required input is stated, and every value has the type of its input and
// is one of its allowed values. The error's path is the name of the input.
func (d *ModuleDescription) InputsProblem(module string, inputs map[string]any) *FieldError {
	for _, name := range Names(inputs) {
		input, exists := d.input(name)
		if !exists {
			has := "the module " + module + " takes no input"
			if names := d.InputNames(); len(names) > 0 {
				has = "the inputs of the module " + module + " are " + strings.Join(names, ", ")
			}
			return &FieldError{name, "unknown field " + fmt.Sprintf("%q", name) + ": " + has}
		}
		if problem := inputTypeProblem(input.Type, inputs[name]); problem != "" {
			return &FieldError{name, problem}
		}
		if len(input.AllowedValues) > 0 && !slices.ContainsFunc(input.AllowedValues, func(allowed any) bool { return sameInputValue(allowed, inputs[name]) }) {
			allowed := []string{}
			for _, value := range input.AllowedValues {
				allowed = append(allowed, fmt.Sprint(value))
			}
			return &FieldError{name, "expected one of " + strings.Join(allowed, ", ")}
		}
	}
	for _, input := range d.Inputs {
		if _, stated := inputs[input.Name]; input.Required && !stated {
			return &FieldError{input.Name, "is required: " + input.Description}
		}
	}
	return nil
}

// inputTypeProblem says what is wrong with a value for an input of a type,
// or "". The value is as YAML or JSON gives it.
func inputTypeProblem(inputType string, value any) string {
	switch inputType {
	case "string":
		if _, ok := value.(string); !ok {
			return "expected a string"
		}
	case "integer":
		if _, ok := asWholeNumber(value); !ok {
			return "expected a whole number"
		}
	case "number":
		if _, ok := asNumber(value); !ok {
			return "expected a number"
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return "expected true or false"
		}
	case "duration":
		// The module gets the text as written, such as 5m, and reads it
		// itself; MMWatchdog checks only that the text is a duration.
		text, ok := value.(string)
		if !ok {
			return "expected a duration such as 30s, 5m, 2h, or 7d"
		}
		if d, e := ParseDuration(text); e != nil || d < 0 {
			return "expected a duration such as 30s, 5m, 2h, or 7d"
		}
	case "list_of_strings":
		list, ok := value.([]any)
		for _, item := range list {
			if _, isString := item.(string); !isString {
				ok = false
			}
		}
		if !ok {
			return "expected a list of strings"
		}
	}
	return ""
}

// asNumber returns a value that YAML or JSON read as a number.
func asNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float64:
		return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	case json.Number:
		f, e := v.Float64()
		return f, e == nil
	}
	return 0, false
}

// asWholeNumber returns a value that YAML or JSON read as a whole number.
func asWholeNumber(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, e := v.Int64()
		return n, e == nil
	case float64:
		return int64(v), v == math.Trunc(v) && math.Abs(v) < 1<<53
	}
	return 0, false
}

// sameInputValue compares an allowed value with a stated one.
func sameInputValue(allowed, stated any) bool {
	if a, ok := asWholeNumber(allowed); ok {
		s, ok := asWholeNumber(stated)
		return ok && a == s
	}
	return allowed == stated
}
