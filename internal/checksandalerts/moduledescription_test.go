package checksandalerts_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

// certificatesDescription is a description that states everything that a
// description of a check module can state.
const certificatesDescription = `summary: Finds the certificates of a directory that expire soon.
used_as: check
inputs:
  certificate_directory:
    type: string
    required: true
    description: The absolute path of the directory that holds the certificates.
  days_before_expiry:
    type: integer
    required: false
    allowed_values: [7, 14, 30]
    description: A certificate that expires within this many days is a finding.
  key_type:
    type: string
    required: false
    allowed_values: [rsa, ecdsa]
    description: Only certificates with a key of this type are checked.
  load_factor:
    type: number
    required: false
    description: A number that is not whole.
  follow_links:
    type: boolean
    required: false
    description: Whether the module follows symbolic links.
  host_names:
    type: list_of_strings
    required: false
    description: Only certificates for these names are checked.
measurements:
  certificates:
    unit: count
    description: The number of certificates that the module found.
  days_left:
    unit: days
    description: The days until the first certificate expires.
one_incident_for_each_finding: true
examples:
  - description: Alert for each certificate that expires within 14 days.
    config: |
      certificates:
        certificate_directory: /etc/ssl/myapp
        days_before_expiry: 14
        run_as_user_account: myapp
      fail_when_measurement_below:
        certificates: 1
      alert_severity: warn
  - description: Check the certificates of two names.
    config: |
      certificates: {certificate_directory: /etc/ssl/myapp, host_names: [www.example.com, api.example.com]}
      alert_severity: sos
`

// describedModule writes a module's description file into a directory of
// its own and reads it as MMWatchdog does.
func describedModule(t *testing.T, name, description string) (*checksandalerts.ModuleDescription, string, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, checksandalerts.ModuleDescriptionFile)
	if e := os.WriteFile(path, []byte(description), 0644); e != nil {
		t.Fatal(e)
	}
	d, e := checksandalerts.ReadModuleDescription(dir, name)
	return d, path, e
}

// A description file states what a module does, what it is used as, its
// inputs with the type of each, its measurements with the unit of each,
// whether it reports findings, and at least one example.
func TestAModuleDescriptionIsRead(t *testing.T) {
	t.Parallel()
	d, _, e := describedModule(t, "certificates", certificatesDescription)
	if e != nil {
		t.Fatal(e)
	}
	want := &checksandalerts.ModuleDescription{
		Summary: "Finds the certificates of a directory that expire soon.", UsedAs: checksandalerts.UsedAsCheck, OneIncidentForEachFinding: true,
		Inputs: []checksandalerts.ModuleInput{
			{Name: "certificate_directory", Type: "string", Required: true, Description: "The absolute path of the directory that holds the certificates."},
			{Name: "days_before_expiry", Type: "integer", AllowedValues: []any{7, 14, 30}, Description: "A certificate that expires within this many days is a finding."},
			{Name: "key_type", Type: "string", AllowedValues: []any{"rsa", "ecdsa"}, Description: "Only certificates with a key of this type are checked."},
			{Name: "load_factor", Type: "number", Description: "A number that is not whole."},
			{Name: "follow_links", Type: "boolean", Description: "Whether the module follows symbolic links."},
			{Name: "host_names", Type: "list_of_strings", Description: "Only certificates for these names are checked."},
		},
		Measurements: []checksandalerts.ModuleMeasurement{{Name: "certificates", Unit: "count", Description: "The number of certificates that the module found."}, {Name: "days_left", Unit: "days", Description: "The days until the first certificate expires."}},
		Examples: []checksandalerts.ModuleExample{
			{Description: "Alert for each certificate that expires within 14 days.", Configuration: "certificates:\n  certificate_directory: /etc/ssl/myapp\n  days_before_expiry: 14\n  run_as_user_account: myapp\nfail_when_measurement_below:\n  certificates: 1\nalert_severity: warn\n"},
			{Description: "Check the certificates of two names.", Configuration: "certificates: {certificate_directory: /etc/ssl/myapp, host_names: [www.example.com, api.example.com]}\nalert_severity: sos\n"},
		},
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("got  %+v\nwant %+v", d, want)
	}
	if unit, listed := d.UnitOf("days_left"); unit != "days" || !listed {
		t.Fatal(unit, listed)
	}
	if _, listed := d.UnitOf("hours_left"); listed || !reflect.DeepEqual(d.MeasurementNames(), []string{"certificates", "days_left"}) || d.InputNames()[0] != "certificate_directory" {
		t.Fatal(d.MeasurementNames(), d.InputNames())
	}
	// The description as "mmwatchdog describe-module --json" writes it.
	written, _ := json.Marshal(d.Inputs[1])
	if string(written) != `{"name":"days_before_expiry","type":"integer","required":false,"allowed_values":[7,14,30],"description":"A certificate that expires within this many days is a finding."}` {
		t.Fatal(string(written))
	}
	// The smallest description: a module without inputs.
	if d, _, e := describedModule(t, "own_alarm", "summary: Rings the bell.\nused_as: alert_destination\nexamples:\n  - description: Ring for every sos alert.\n    config: |\n      own_alarm: {}\n      alert_severities: [sos]\n"); e != nil || len(d.Inputs) != 0 || d.UsedAs != checksandalerts.UsedAsAlertDestination {
		t.Fatal(d, e)
	}
}

// A description file is strict: every field that is required must be there,
// a field that MMWatchdog does not know is refused, and every error names the file
// and the line: the line of the field that is wrong, or the first line of
// the mapping that lacks a field.
func TestAModuleDescriptionThatIsNotValidIsRefused(t *testing.T) {
	t.Parallel()
	const valid = `summary: Counts the jobs that wait in a queue file.
used_as: check
inputs:
  queue_file_path:
    type: string
    required: true
    description: The file that holds the number of waiting jobs.
measurements:
  queued_jobs:
    unit: count
    description: The number of jobs that wait.
examples:
  - description: Fail when more than 500 jobs wait.
    config: |
      queued_jobs:
        queue_file_path: /var/lib/myapp/pending
      alert_severity: warn
`
	if _, _, e := describedModule(t, "queued_jobs", valid); e != nil {
		t.Fatal(e)
	}
	fields := "; the fields are summary, used_as, inputs, measurements, one_incident_for_each_finding, service_keeps_incidents, examples"
	example := "examples:\n  - description: Fail when more than 500 jobs wait.\n    config: |\n      queued_jobs:\n        queue_file_path: /var/lib/myapp/pending\n      alert_severity: warn\n"
	for _, tc := range []struct{ name, from, to, want string }{
		{"no summary", "summary: Counts the jobs that wait in a queue file.\n", "", ":1: the description: summary is required"},
		{"an empty summary", "summary: Counts the jobs that wait in a queue file.", "summary: ''", ":1: the description: summary: expected a text"},
		{"a summary that is no text", "summary: Counts the jobs that wait in a queue file.", "summary: 7", ":1: the description: summary: expected a text"},
		{"no used_as", "used_as: check\n", "", ":1: the description: used_as is required"},
		{"another used_as", "used_as: check", "used_as: recovery_command", ":2: used_as: expected check or alert_destination"},
		{"a field that MMWatchdog does not know", "used_as: check", "used_as: check\nversion: 2", `:3: the description: unknown field "version"` + fields},
		{"the old name of a field", "used_as: check", "kind: check", `:2: the description: unknown field "kind"` + fields},
		{"a field twice", "used_as: check", "used_as: check\nsummary: Again.", `:3: the description: expected each key once, as a plain string: "summary"`},
		{"inputs as a list", "inputs:\n  queue_file_path:\n    type: string\n    required: true\n    description: The file that holds the number of waiting jobs.", "inputs: [queue_file_path]", ":3: inputs: expected a mapping"},
		{"an input without a type", "    type: string\n", "", ":5: inputs.queue_file_path: type is required"},
		{"a type that MMWatchdog does not have", "type: string", "type: path", ":5: inputs.queue_file_path: type: expected one of string, integer, number, boolean, duration, list_of_strings"},
		{"a type of a core module only", "type: string", "type: list_of_integers", ":5: inputs.queue_file_path: type: expected one of string, integer, number, boolean, duration, list_of_strings"},
		{"an input that does not say whether it is required", "    required: true\n", "", ":5: inputs.queue_file_path: set required to true for a required input or false for an optional input"},
		{"required that is neither true nor false", "required: true", "required: sometimes", ":6: inputs.queue_file_path: required: expected true or false"},
		{"an input without a description", "    description: The file that holds the number of waiting jobs.\n", "", ":5: inputs.queue_file_path: description is required"},
		{"an input with a field that MMWatchdog does not know", "required: true", "required: true\n    default: /var/lib/myapp/pending", `:7: inputs.queue_file_path: unknown field "default"; the fields are type, required, allowed_values, description`},
		{"an input with the name of a run setting", "  queue_file_path:", "  run_as_user_account:", ":5: inputs.run_as_user_account: no input can have this name: run_as_user_account says how MMWatchdog runs every module that is not core"},
		{"an input named env", "  queue_file_path:", "  env:", ":5: inputs.env: no input can have this name: env says how MMWatchdog runs every module that is not core"},
		{"an input with a name that is none", "  queue_file_path:", "  Queue-File:", ":5: inputs.Queue-File: the name of an input uses lower-case letters, digits, and underscores, and starts with a letter"},
		{"allowed values of an input that takes none", "type: string\n    required: true", "type: boolean\n    required: true\n    allowed_values: [true]", ":7: inputs.queue_file_path: allowed_values: only an input of the type string or integer takes a list of allowed values"},
		{"no allowed value", "required: true", "required: true\n    allowed_values: []", ":7: inputs.queue_file_path: allowed_values: expected a list of at least one value"},
		{"an allowed value of another type", "required: true", "required: true\n    allowed_values: [/a, 7]", ":7: inputs.queue_file_path: allowed_values: expected a string"},
		{"a measurement without a unit", "    unit: count\n", "", ":10: measurements.queued_jobs: unit is required"},
		{"a measurement without a description", "    description: The number of jobs that wait.\n", "", ":10: measurements.queued_jobs: description is required"},
		{"a measurement with a field that MMWatchdog does not know", "unit: count", "unit: count\n    value: 780", `:11: measurements.queued_jobs: unknown field "value"; the fields are unit, description`},
		{"a measurement with a name that is none", "  queued_jobs:\n    unit", "  queued jobs:\n    unit", ":10: measurements.queued jobs: the name of a measurement uses lower-case letters, digits, and underscores, and starts with a letter"},
		{"a unit that is too long", "unit: count", "unit: " + strings.Repeat("u", 65), ":10: measurements.queued_jobs: unit: expected at most 64 bytes"},
		{"measurements of an alert destination", "used_as: check", "used_as: alert_destination", ":9: measurements: only a module that is used as a check reports measurements"},
		{"findings that are neither true nor false", "examples:", "one_incident_for_each_finding: sometimes\nexamples:", ":12: one_incident_for_each_finding: expected true or false"},
		{"a check whose service keeps incidents", "examples:", "service_keeps_incidents: true\nexamples:", ":12: service_keeps_incidents: only a module that is used as an alert destination delivers to a service"},
		{"no examples", example, "", ":1: examples: is required: a list of at least one example, each with a description and a config"},
		{"an empty list of examples", example, "examples: []\n", ":1: examples: is required: a list of at least one example, each with a description and a config"},
		{"an example without a description", "  - description: Fail when more than 500 jobs wait.\n    config", "  - config", ":13: examples.0: description is required"},
		{"an example without a config", "    config: |\n      queued_jobs:\n        queue_file_path: /var/lib/myapp/pending\n      alert_severity: warn\n", "", ":13: examples.0: config is required"},
		{"an example with a field that MMWatchdog does not know", "    config: |", "    module: queued_jobs\n    config: |", `:14: examples.0: unknown field "module"; the fields are description, config`},
		{"an example that does not name the module", "      queued_jobs:\n", "      queue:\n", ": examples.0.config: the example does not name the module queued_jobs"},
		{"an example with an input that the module does not have", "        queue_file_path: /var/lib/myapp/pending", "        queue: /var/lib/myapp/pending", `: examples.0.config: queued_jobs.queue: unknown field "queue": the inputs of the module queued_jobs are queue_file_path`},
		{"an example without a required input", "      queued_jobs:\n        queue_file_path: /var/lib/myapp/pending", "      queued_jobs: {}", ": examples.0.config: queued_jobs.queue_file_path: is required: The file that holds the number of waiting jobs."},
		{"an example with a value of another type", "queue_file_path: /var/lib/myapp/pending", "queue_file_path: 7", ": examples.0.config: queued_jobs.queue_file_path: expected a string"},
		{"an example whose module has no mapping", "      queued_jobs:\n        queue_file_path: /var/lib/myapp/pending", "      queued_jobs: /var/lib/myapp/pending", ": examples.0.config: queued_jobs: expected a mapping of the module's inputs"},
		{"an example that is no mapping", "    config: |\n      queued_jobs:\n        queue_file_path: /var/lib/myapp/pending\n      alert_severity: warn", "    config: queued_jobs", ": examples.0.config: expected the YAML of one check or one destination that names the module queued_jobs"},
		{"a second example that is not valid", example, example + "  - description: Another.\n    config: |\n      queued_jobs: {queue_file_path: [a]}\n", ": examples.1.config: queued_jobs.queue_file_path: expected a string"},
		{"a list in place of the description", valid, "- summary: Counts the jobs.\n", ":1: the description: expected a mapping"},
		{"an empty file", valid, "", ": expected a mapping"},
	} {
		if !strings.Contains(valid, tc.from) {
			t.Fatalf("%s: the valid description has no %q", tc.name, tc.from)
		}
		_, path, e := describedModule(t, "queued_jobs", strings.Replace(valid, tc.from, tc.to, 1))
		if e == nil || e.Error() != path+tc.want {
			t.Errorf("%s:\n got %v\nwant %s", tc.name, e, path+tc.want)
		}
	}
	// A destination module can say that its service keeps incidents of its
	// own; that is true or false.
	keeps, _, e := describedModule(t, "own_alarm", "summary: Rings the bell.\nused_as: alert_destination\nservice_keeps_incidents: true\nexamples:\n  - description: Ring.\n    config: |\n      own_alarm: {}\n")
	if e != nil || !keeps.ServiceKeepsIncidents {
		t.Fatal(keeps, e)
	}
	if _, path, e := describedModule(t, "own_alarm", "summary: Rings the bell.\nused_as: alert_destination\nservice_keeps_incidents: often\nexamples:\n  - description: Ring.\n    config: |\n      own_alarm: {}\n"); e == nil || e.Error() != path+":3: service_keeps_incidents: expected true or false" {
		t.Fatal(e)
	}
	// A module that delivers alerts reports no findings.
	if _, path, e := describedModule(t, "own_alarm", "summary: Rings the bell.\nused_as: alert_destination\none_incident_for_each_finding: true\nexamples:\n  - description: Ring.\n    config: |\n      own_alarm: {}\n"); e == nil || e.Error() != path+":3: one_incident_for_each_finding: only a module that is used as a check reports findings" {
		t.Fatal(e)
	}
	// Text that is no YAML, and a file that is larger than MMWatchdog reads.
	if _, path, e := describedModule(t, "queued_jobs", "summary: [unclosed\n"); e == nil || !strings.HasPrefix(e.Error(), path+": yaml: ") {
		t.Fatal(e)
	}
	if _, path, e := describedModule(t, "queued_jobs", valid+"# "+strings.Repeat("x", checksandalerts.ModuleDescriptionLimit)+"\n"); e == nil || !strings.HasPrefix(e.Error(), path+" holds more than 262144 bytes") {
		t.Fatal(e)
	}
}

// MMWatchdog checks what a check or a destination states for a module against the
// module's description: every input is one that the module has, every
// required input is stated, and every value has the type of its input and is
// one of its allowed values. The values are as YAML gives them, and as JSON
// gives them on a joined node and in a queued delivery.
func TestTheInputsOfACallAreCheckedAgainstTheDescription(t *testing.T) {
	t.Parallel()
	d, _, e := describedModule(t, "certificates", certificatesDescription)
	if e != nil {
		t.Fatal(e)
	}
	required := map[string]any{"certificate_directory": "/etc/ssl/myapp"}
	with := func(name string, value any) map[string]any {
		return map[string]any{"certificate_directory": "/etc/ssl/myapp", name: value}
	}
	for _, valid := range []map[string]any{
		required,
		with("days_before_expiry", 14), with("days_before_expiry", int64(14)), with("days_before_expiry", 14.0), with("days_before_expiry", json.Number("14")),
		with("key_type", "ecdsa"),
		with("load_factor", 1.5), with("load_factor", 2), with("load_factor", int64(2)), with("load_factor", uint64(2)), with("load_factor", json.Number("1.5e3")),
		with("follow_links", true), with("follow_links", false),
		with("host_names", []any{"www.example.com", "api.example.com"}), with("host_names", []any{}),
	} {
		if problem := d.InputsProblem("certificates", valid); problem != nil {
			t.Errorf("%v: %v", valid, problem)
		}
	}
	inputs := "the inputs of the module certificates are certificate_directory, days_before_expiry, key_type, load_factor, follow_links, host_names"
	for _, tc := range []struct {
		inputs        map[string]any
		path, message string
	}{
		{map[string]any{}, "certificate_directory", "is required: The absolute path of the directory that holds the certificates."},
		{map[string]any{"days_before_expiry": 14}, "certificate_directory", "is required: The absolute path of the directory that holds the certificates."},
		{with("certificate_dir", "/etc/ssl"), "certificate_dir", `unknown field "certificate_dir": ` + inputs},
		{with("inputs", map[string]any{"key_type": "rsa"}), "inputs", `unknown field "inputs": ` + inputs},
		{map[string]any{"certificate_directory": 7}, "certificate_directory", "expected a string"},
		{map[string]any{"certificate_directory": nil}, "certificate_directory", "expected a string"},
		{map[string]any{"certificate_directory": []any{"/etc/ssl/myapp"}}, "certificate_directory", "expected a string"},
		{with("days_before_expiry", "14"), "days_before_expiry", "expected a whole number"},
		{with("days_before_expiry", 14.5), "days_before_expiry", "expected a whole number"},
		{with("days_before_expiry", json.Number("14.5")), "days_before_expiry", "expected a whole number"},
		{with("days_before_expiry", true), "days_before_expiry", "expected a whole number"},
		{with("days_before_expiry", 15), "days_before_expiry", "expected one of 7, 14, 30"},
		{with("days_before_expiry", json.Number("15")), "days_before_expiry", "expected one of 7, 14, 30"},
		{with("key_type", "dsa"), "key_type", "expected one of rsa, ecdsa"},
		{with("key_type", "RSA"), "key_type", "expected one of rsa, ecdsa"},
		{with("load_factor", "1.5"), "load_factor", "expected a number"},
		{with("load_factor", false), "load_factor", "expected a number"},
		{with("follow_links", "true"), "follow_links", "expected true or false"},
		{with("follow_links", 1), "follow_links", "expected true or false"},
		{with("host_names", "www.example.com"), "host_names", "expected a list of strings"},
		{with("host_names", []any{"www.example.com", 7}), "host_names", "expected a list of strings"},
		{with("host_names", map[string]any{"www": "example.com"}), "host_names", "expected a list of strings"},
	} {
		if problem := d.InputsProblem("certificates", tc.inputs); problem == nil || problem.Path != tc.path || problem.Message != tc.message {
			t.Errorf("%v:\n got %v\nwant %s: %s", tc.inputs, problem, tc.path, tc.message)
		}
	}
	none, _, e := describedModule(t, "own_alarm", testhelpers.TestModuleDescription("own_alarm", checksandalerts.UsedAsAlertDestination, nil, nil))
	if e != nil {
		t.Fatal(e)
	}
	if problem := none.InputsProblem("own_alarm", map[string]any{"bell": "loud"}); problem == nil || problem.Message != `unknown field "bell": the module own_alarm takes no input` {
		t.Fatal(problem)
	}
}

// An input of the type duration takes a time as MMWatchdog's own settings write it,
// such as 5m, and the module gets the text as written.
func TestADurationInputIsChecked(t *testing.T) {
	t.Parallel()
	d, _, e := describedModule(t, "own_alarm", "summary: Rings the bell.\nused_as: alert_destination\ninputs:\n  ring_again_every:\n    type: duration\n    required: true\n    description: How long the bell waits before it rings again.\nexamples:\n  - description: Ring.\n    config: |\n      own_alarm: {ring_again_every: 5m}\n")
	if e != nil {
		t.Fatal(e)
	}
	for _, valid := range []string{"30s", "5m", "1h30m", "7d", "0s"} {
		if problem := d.InputsProblem("own_alarm", map[string]any{"ring_again_every": valid}); problem != nil {
			t.Errorf("%s: %v", valid, problem)
		}
	}
	for _, wrong := range []any{30, "30", "five minutes", "-5m", "5 m", true} {
		if problem := d.InputsProblem("own_alarm", map[string]any{"ring_again_every": wrong}); problem == nil || problem.Path != "ring_again_every" || problem.Message != "expected a duration such as 30s, 5m, 2h, or 7d" {
			t.Errorf("%v: %v", wrong, problem)
		}
	}
}

// mmwatchdog validate checks every check against the description of its module,
// with the file and the line: an input that the module does not have, a
// required input that is missing, a value of the wrong type or that is not
// allowed, a module that is used as the wrong thing, a limit on a
// measurement that the module does not report, and a check without an alert
// severity. A name that no module has is an unknown field, and the error
// says where MMWatchdog looked.
func TestACheckIsValidatedAgainstTheDescriptionOfItsModule(t *testing.T) {
	t.Parallel()
	modules := t.TempDir()
	testhelpers.InstallStarlarkModule(t, modules, "certificates", certificatesDescription, testhelpers.ScriptOfACheck)
	testhelpers.InstallStarlarkModule(t, modules, "own_alarm", testhelpers.TestModuleDescription("own_alarm", checksandalerts.UsedAsAlertDestination, nil, nil), testhelpers.ScriptOfADestination)
	load := func(check string) (*config.Config, string, error) {
		directory := testhelpers.ConfigFileWithoutSeverities(t, "keep_undelivered_alerts_for: 24h\nundelivered_alerts_content_size_limit: 8MiB\nalert_severity_when_this_node_goes_silent: sos\nalert_severity_when_this_node_has_rebooted: info\nkeep_measurement_history_for: 7d\nmeasurement_history_size_limit: 256MiB\nexternal_module_dir: "+modules+"\ncheck_groups:\n  web:\n    checks:\n      certificates_are_valid:\n"+check)
		c, e := config.Load(directory)
		return c, filepath.Join(directory, "check_groups", "checks.yml"), e
	}
	c, _, e := load("        certificates:\n          certificate_directory: /etc/ssl/myapp\n          days_before_expiry: 14\n          host_names: [www.example.com]\n          run_as_user_account: myapp\n          run_from_directory: /var/lib/myapp\n          env: {SSL_CERT_DIR: /etc/ssl/certs}\n        fail_when_measurement_below:\n          certificates: 1\n        alert_severity: warn\n")
	if e != nil {
		t.Fatal(e)
	}
	check := c.CheckGroups["web"].Checks["certificates_are_valid"]
	call := check.Inputs.(*checksandalerts.ModuleCall)
	if check.ModuleName() != "certificates" || check.ModuleKind() != checksandalerts.ModuleKindProjectStarlark || !check.Module().OneIncidentForEachFinding || !check.Module().LimitsOnItsMeasurements ||
		!reflect.DeepEqual(call.Inputs, map[string]any{"certificate_directory": "/etc/ssl/myapp", "days_before_expiry": 14, "host_names": []any{"www.example.com"}}) ||
		!reflect.DeepEqual(call.ExecOptions, primitives.ExecOptions{RunAsUserAccount: "myapp", RunFromDirectory: "/var/lib/myapp", Env: map[string]string{"SSL_CERT_DIR": "/etc/ssl/certs"}}) ||
		!reflect.DeepEqual(check.MeasurementNames(), []string{"certificates", "days_left"}) {
		t.Fatalf("%+v %+v", check, call)
	}
	at := "check_groups.web.checks.certificates_are_valid"
	for _, tc := range []struct {
		check string
		line  int
		want  string
	}{
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n          key_typ: rsa\n        alert_severity: warn\n", 12, at + `.certificates.key_typ: unknown field "key_typ": the inputs of the module certificates are certificate_directory, days_before_expiry, key_type, load_factor, follow_links, host_names`},
		{"        certificates:\n          key_type: rsa\n        alert_severity: warn\n", 10, at + ".certificates.certificate_directory: is required: The absolute path of the directory that holds the certificates."},
		{"        certificates: {}\n        alert_severity: warn\n", 10, at + ".certificates.certificate_directory: is required: The absolute path of the directory that holds the certificates."},
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n          days_before_expiry: two weeks\n        alert_severity: warn\n", 12, at + ".certificates.days_before_expiry: expected a whole number"},
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n          days_before_expiry: 10\n        alert_severity: warn\n", 12, at + ".certificates.days_before_expiry: expected one of 7, 14, 30"},
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n          follow_links: 'yes'\n        alert_severity: warn\n", 12, at + ".certificates.follow_links: expected true or false"},
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n          host_names: www.example.com\n        alert_severity: warn\n", 12, at + ".certificates.host_names: expected a list of strings"},
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n          load_factor: high\n        alert_severity: warn\n", 12, at + ".certificates.load_factor: expected a number"},
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n          run_from_directory: lib\n        alert_severity: warn\n", 12, at + ".certificates.run_from_directory: must be absolute"},
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n          env: {MMWATCHDOG_MODULE_HELPER: /bin/false}\n        alert_severity: warn\n", 12, at + ".certificates.env.MMWATCHDOG_MODULE_HELPER: unsupported environment variable"},
		// A run setting has the type that it has in a shell check, and the
		// error names its line.
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n          run_as_user_account: 7\n        alert_severity: warn\n", 12, at + ".certificates.run_as_user_account: expected a string, found the whole number 7"},
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n          run_as_user_account: [mmwatchdog]\n        alert_severity: warn\n", 12, at + ".certificates.run_as_user_account: expected a string, found a list"},
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n          run_from_directory: true\n        alert_severity: warn\n", 12, at + ".certificates.run_from_directory: expected a string, found true"},
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n          env: {WORKERS: 4}\n        alert_severity: warn\n", 12, at + ".certificates.env.WORKERS: expected a string, found the whole number 4"},
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n          env: [WORKERS]\n        alert_severity: warn\n", 12, at + ".certificates.env: expected a mapping, found a list"},
		{"        own_alarm: {}\n        alert_severity: warn\n", 10, at + ".own_alarm: the module own_alarm is used as an alert destination, not as a check: its description says used_as: alert_destination"},
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n        fail_when_measurement_above:\n          hours_left: 12\n        alert_severity: warn\n", 13, at + ".fail_when_measurement_above.hours_left: this check has no such measurement; it measures certificates, days_left"},
		// The module can answer failed by itself, so its check always
		// states a severity, also without a limit.
		{"        certificates:\n          certificate_directory: /etc/ssl/myapp\n", 9, at + ".alert_severity: required: info, warn, or sos"},
	} {
		_, file, e := load(tc.check)
		if e == nil || !strings.HasPrefix(e.Error(), file+":"+strconv.Itoa(tc.line+2)+":") || !strings.HasSuffix(e.Error(), ": "+tc.want) {
			t.Errorf("%s:\n got %v\nwant line %d: %s", tc.check, e, tc.line, tc.want)
		}
	}
	// A name that no module has: the error starts with the unknown field
	// and says where MMWatchdog looked.
	_, file, e := load("        certificate:\n          certificate_directory: /etc/ssl/myapp\n        alert_severity: warn\n")
	if want := file + `:12:9: ` + at + `.certificate: unknown field "certificate": the field name matches neither a setting of a check nor an installed module. MMWatchdog has no core module of that name, and no directory of that name is in `; e == nil || !strings.HasPrefix(e.Error(), want) || !strings.HasSuffix(e.Error(), ", which does not exist or in "+modules+" (certificates, own_alarm)") {
		t.Fatalf("got %v\nwant %s", e, want)
	}
	// Two modules that are not core are two modules too.
	if _, file, e = load("        certificates: {certificate_directory: /etc/ssl/myapp}\n        own_alarm: {}\n        alert_severity: warn\n"); e == nil || e.Error() != file+":11:7: "+at+": names the modules certificates and own_alarm: a check names exactly one module" {
		t.Fatal(e)
	}
}

// yamlTypeOfInput is the type that a description states for an input whose
// Go type is t.
func yamlTypeOfInput(t reflect.Type) string {
	switch {
	case t == reflect.TypeOf(checksandalerts.Duration("")):
		return "duration"
	case t.Kind() == reflect.String:
		return "string"
	case t.Kind() == reflect.Int:
		return "integer"
	case t.Kind() == reflect.Bool:
		return "boolean"
	case t == reflect.TypeOf([]string{}):
		return "list_of_strings"
	case t == reflect.TypeOf([]int{}):
		return "list_of_integers"
	case t == reflect.TypeOf(map[string]string{}):
		return "mapping_of_strings"
	}
	return "no type that a description has: " + t.String()
}

// The description of every core module is built in, beside the module's
// code, and says what the code takes: it lists exactly the inputs of the
// module's input type, each with its type, and a module that takes one
// scalar describes that value. So "mmwatchdog describe-module" never shows an input
// that the configuration loader refuses, and never leaves one out.
func TestTheDescriptionOfEveryCoreModuleListsItsInputs(t *testing.T) {
	t.Parallel()
	type described struct {
		name            string
		usedAs          string
		description     *checksandalerts.ModuleDescription
		inputs          reflect.Type
		scalarStandsFor string
	}
	modules := []described{}
	for _, m := range checksandalerts.CoreCheckModules {
		modules = append(modules, described{m.Name, checksandalerts.UsedAsCheck, m.Description, reflect.TypeOf(m.NewInputs()).Elem(), m.ScalarStandsFor})
	}
	for _, m := range checksandalerts.CoreDestinationModules {
		modules = append(modules, described{m.Name, checksandalerts.UsedAsAlertDestination, m.Description, reflect.TypeOf(m.NewInputs()).Elem(), ""})
	}
	types := append(slices.Clone(checksandalerts.ModuleInputTypes), checksandalerts.CoreModuleInputTypes...)
	for _, m := range modules {
		d := m.description
		if d == nil || strings.TrimSpace(d.Summary) == "" || d.UsedAs != m.usedAs || len(d.Examples) == 0 {
			t.Errorf("%s: the description lacks its summary, what the module is used as, or an example: %+v", m.name, d)
			continue
		}
		stated := []checksandalerts.ModuleInput{}
		if d.Value != nil {
			stated = append(stated, *d.Value)
		}
		for _, input := range append(stated, d.Inputs...) {
			if !slices.Contains(types, input.Type) || strings.TrimSpace(input.Description) == "" {
				t.Errorf("%s: the input %q has the type %q or no description", m.name, input.Name, input.Type)
			}
		}
		for _, measurement := range d.Measurements {
			if !checksandalerts.ModuleNamePattern.MatchString(measurement.Name) || measurement.Unit == "" || measurement.Description == "" || m.usedAs != checksandalerts.UsedAsCheck {
				t.Errorf("%s: the measurement %+v", m.name, measurement)
			}
		}
		if m.inputs.Kind() != reflect.Struct {
			// The module's value is one string, such as the address of tcp.
			scalarType := map[reflect.Kind]string{reflect.String: "string"}[m.inputs.Kind()]
			if scalarType == "" || d.Value == nil || d.Value.Type != scalarType || d.Value.Name != "" || !d.Value.Required || len(d.Inputs) != 0 {
				t.Errorf("%s: a module whose value is one scalar describes that value and no inputs: %+v", m.name, d)
			}
			continue
		}
		// A scalar in place of the mapping of inputs, such as the script of
		// shell, is described only for a module that takes one.
		if (d.Value != nil) != (m.scalarStandsFor != "") {
			t.Errorf("%s: the description has a value, or the module takes a scalar, and not both", m.name)
		}
		takes := map[string]string{}
		for name, goType := range checksandalerts.Fields(m.inputs) {
			takes[name] = yamlTypeOfInput(goType)
		}
		describes := map[string]string{}
		for _, input := range d.Inputs {
			if _, twice := describes[input.Name]; twice {
				t.Errorf("%s: the description lists the input %s twice", m.name, input.Name)
			}
			describes[input.Name] = input.Type
		}
		if !reflect.DeepEqual(describes, takes) {
			t.Errorf("%s:\nthe description lists %v\nthe module takes     %v", m.name, describes, takes)
		}
	}
	// A measurement of a description is one that the module can report.
	everyResource := func(measurements map[string][]string) []string {
		all := []string{}
		for _, resource := range checksandalerts.Names(measurements) {
			all = append(all, measurements[resource]...)
		}
		return all
	}
	for name, measured := range map[string][]string{
		"node_filesystem": checksandalerts.NodeFilesystemMeasurements, "every_disk_and_network_filesystem": checksandalerts.NodeFilesystemMeasurements,
		"node_disk_device": checksandalerts.NodeDiskDeviceMeasurements, "node_network_interface": checksandalerts.NodeNetworkInterfaceMeasurements,
		"node_resource": everyResource(checksandalerts.NodeResourceMeasurements), "systemd_unit_resource": everyResource(checksandalerts.SystemdUnitResourceMeasurements),
	} {
		for _, measurement := range checksandalerts.CheckModulesByName[name].Description.MeasurementNames() {
			if !slices.Contains(measured, measurement) {
				t.Errorf("%s: the description lists the measurement %s, which the module never reports", name, measurement)
			}
		}
	}
}

// Every example of a core module's description is a valid check or a valid
// destination as it is written, so a reader of "mmwatchdog describe-module" can
// copy it.
func TestTheExamplesOfEveryCoreModuleAreValid(t *testing.T) {
	t.Parallel()
	none := t.TempDir()
	modules, e := config.ListModules(checksandalerts.ModuleDirs{OfficialExtra: filepath.Join(none, "official_extra_modules"), External: filepath.Join(none, "external_modules")})
	if e != nil || len(modules) != len(checksandalerts.CoreCheckModules)+len(checksandalerts.CoreDestinationModules) {
		t.Fatal(modules, e)
	}
	for _, module := range modules {
		for _, example := range module.Description.Examples {
			if strings.TrimSpace(example.Description) == "" || !strings.HasPrefix(example.Configuration, module.Name+":") {
				t.Errorf("%s: an example has no description, or does not start with the module: %+v", module.Name, example)
			}
			if e := testhelpers.LoadExample(t, checksandalerts.ModuleDirs{OfficialExtra: filepath.Join(none, "official_extra_modules"), External: filepath.Join(none, "external_modules")}, module.Description.UsedAs, example); e != nil {
				t.Errorf("%s: the example %q is not valid: %v", module.Name, example.Description, e)
			}
		}
	}
}

// The official extra modules of this repository are ones that MMWatchdog can use:
// each has its script and a description that MMWatchdog reads, the directory
// carries the version of this mmwatchdog program, and each example is a valid
// destination or a valid check.
func TestTheOfficialExtraModulesOfTheRepositoryAreValid(t *testing.T) {
	t.Parallel()
	testhelpers.NeedSourceCheckout(t)
	official, e := filepath.Abs("../../official_extra_modules")
	if e != nil {
		t.Fatal(e)
	}
	dirs := checksandalerts.ModuleDirs{OfficialExtra: official, External: filepath.Join(t.TempDir(), "external_modules")}
	catalog, e := checksandalerts.LoadModuleCatalog(dirs)
	if e != nil {
		t.Fatal(e)
	}
	if !slices.Contains(catalog.OfficialExtra, "slack") || !slices.Contains(catalog.OfficialExtra, "telegram") {
		t.Fatal("the official extra modules lack slack or telegram:", catalog.OfficialExtra)
	}
	for _, name := range catalog.OfficialExtra {
		module, e := catalog.Module(name)
		if e != nil {
			t.Errorf("%s: %v", name, e)
			continue
		}
		if module.Kind != checksandalerts.ModuleKindOfficialExtra || !module.IsStarlark() || module.Directory != filepath.Join(official, name) {
			t.Errorf("%s: %+v", name, module)
		}
		for _, example := range module.Description.Examples {
			if e := testhelpers.LoadExample(t, dirs, module.Description.UsedAs, example); e != nil {
				t.Errorf("%s: the example %q is not valid: %v", name, example.Description, e)
			}
		}
	}
	// The destination modules of release 1 state these inputs.
	for name, want := range map[string][]string{
		"discord":    {"webhook_url_file", "mention"},
		"mattermost": {"webhook_url_file", "mention"},
		"pushover":   {"application_token_file", "user_or_group_key_file", "repeat_sos_alerts_every", "stop_repeating_sos_alerts_after", "api_server_url"},
		"slack":      {"webhook_url_file", "mention"},
		"telegram":   {"bot_token_file", "chat_id", "bot_api_server_url"},
	} {
		if module, e := catalog.Module(name); e != nil || module.Description.UsedAs != checksandalerts.UsedAsAlertDestination || !reflect.DeepEqual(module.Description.InputNames(), want) {
			t.Errorf("%s: %v", name, e)
		}
	}
}

// A module that takes no input is called with nothing below its name, or
// with an empty mapping. Both are the same check.
func TestAModuleWithoutInputsIsCalledWithNothingBelowItsName(t *testing.T) {
	t.Parallel()
	modules := t.TempDir()
	testhelpers.InstallStarlarkModule(t, modules, "uptime", testhelpers.TestModuleDescription("uptime", checksandalerts.UsedAsCheck, nil, nil), testhelpers.ScriptOfACheck)
	identities := []string{}
	for _, call := range []string{"uptime:", "uptime: {}", "uptime: null", "uptime: ~"} {
		directory := testhelpers.ConfigFileWithoutSeverities(t, "keep_undelivered_alerts_for: 24h\nundelivered_alerts_content_size_limit: 8MiB\nalert_severity_when_this_node_goes_silent: sos\nalert_severity_when_this_node_has_rebooted: info\nkeep_measurement_history_for: 7d\nmeasurement_history_size_limit: 256MiB\nexternal_module_dir: "+modules+"\ncheck_groups:\n  node:\n    checks:\n      node_is_up:\n        "+call+"\n        alert_severity: warn\n")
		c, e := config.Load(directory)
		if e != nil {
			t.Fatalf("%s: %v", call, e)
		}
		check := c.CheckGroups["node"].Checks["node_is_up"]
		if called := check.Inputs.(*checksandalerts.ModuleCall); called.Name != "uptime" || len(called.Inputs) != 0 || called.RunAsUserAccount != "mmwatchdog" || called.RunFromDirectory != "/" {
			t.Fatalf("%s: %+v", call, called)
		}
		identities = append(identities, config.Definition{CheckGroup: "node", Name: "node_is_up", Check: check.Check}.Identity())
	}
	for _, identity := range identities {
		if identity != identities[0] {
			t.Fatal("two ways to call a module without inputs are two checks:", identities)
		}
	}
}

// Every core module names one of CoreModuleTopics in its registry line, and
// every topic has a module, so the index of the module pages and the list
// of "mmwatchdog describe-module" show every module under a topic and no empty one.
func TestEveryCoreModuleNamesATopicOfTheList(t *testing.T) {
	t.Parallel()
	used := map[string]bool{}
	for _, m := range config.CoreModules() {
		if !slices.Contains(checksandalerts.CoreModuleTopics, m.Topic) {
			t.Errorf("%s: the registry line names the topic %q, which is not in CoreModuleTopics %q", m.Name, m.Topic, checksandalerts.CoreModuleTopics)
		}
		used[m.Topic] = true
	}
	for _, topic := range checksandalerts.CoreModuleTopics {
		if !used[topic] {
			t.Errorf("no core module names the topic %q of CoreModuleTopics", topic)
		}
	}
}

// The code file that CoreModules names for each core module exists and
// declares the description that the module's registry line points to, so
// the first line of the generated page names the right file.
func TestEveryCoreModuleHasItsDescriptionInItsCodeFile(t *testing.T) {
	t.Parallel()
	testhelpers.NeedSourceCheckout(t)
	files := gotoken.NewFileSet()
	// The registries and the code of the core modules are in the package
	// checksandalerts.
	parse := func(file string) *ast.File {
		t.Helper()
		parsed, e := parser.ParseFile(files, filepath.Join("../checksandalerts", file), nil, parser.SkipObjectResolution)
		if e != nil {
			t.Fatal(e)
		}
		return parsed
	}
	// The registry lines: the name of each module and the variable that
	// holds its description.
	descriptionVariables := map[string]string{}
	for _, registry := range []string{"checkmodules.go", "destinationmodules.go"} {
		ast.Inspect(parse(registry), func(node ast.Node) bool {
			line, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			name, variable := "", ""
			for _, element := range line.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, _ := pair.Key.(*ast.Ident)
				switch {
				case key == nil:
				case key.Name == "Name":
					if text, ok := pair.Value.(*ast.BasicLit); ok {
						name, _ = strconv.Unquote(text.Value)
					}
				case key.Name == "Description":
					if pointer, ok := pair.Value.(*ast.UnaryExpr); ok {
						if identifier, ok := pointer.X.(*ast.Ident); ok {
							variable = identifier.Name
						}
					}
				}
			}
			if name != "" && variable != "" {
				descriptionVariables[name] = variable
			}
			return true
		})
	}
	for _, m := range config.CoreModules() {
		variable := descriptionVariables[m.Name]
		if variable == "" {
			t.Errorf("%s: no registry line names the module with a description", m.Name)
			continue
		}
		if _, e := os.Stat(filepath.Join("../checksandalerts", m.CodeFile)); e != nil {
			t.Errorf("%s: the code file %s: %v", m.Name, m.CodeFile, e)
			continue
		}
		declared := false
		for _, declaration := range parse(m.CodeFile).Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != gotoken.VAR {
				continue
			}
			for _, spec := range general.Specs {
				for _, identifier := range spec.(*ast.ValueSpec).Names {
					declared = declared || identifier.Name == variable
				}
			}
		}
		if !declared {
			t.Errorf("%s: the code file %s does not declare %s, the description that the registry line names", m.Name, m.CodeFile, variable)
		}
	}
}
