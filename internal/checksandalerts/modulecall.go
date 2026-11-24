package checksandalerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"mmwatchdog/internal/primitives"
)

// A check or a destination calls a module that is not core by the module's
// name, like a core module: the name is the key, and the mapping below it
// holds the module's inputs and, beside them, how MMWatchdog runs the module.
// ModuleCall is that mapping. It is the input type of every official extra
// module and of every module of the project's own, for a check and for a
// destination. MMWatchdog runs such a module in a process of its own and speaks the
// JSON protocol with it: a Starlark script through "mmwatchdog run-starlark-module"
// (starlark.go), an executable directly. The rules are in
// docs/changing-mmwatchdog/design/checks.md under "Modules" and in docs/changing-mmwatchdog/design/alerts.md under
// "Destination modules".

// ModuleFileLimit is the most bytes MMWatchdog reads from a file that an input of a
// module names, such as a credential.
const ModuleFileLimit = 16 * 1024

// ModuleFilesLimit is the most bytes that the files of one destination's
// inputs hold together, and ModuleInputsLimit the most bytes that the
// destination's inputs are as JSON. The two limits keep a request within
// ModuleRequestLimit whatever the alert is:
//   - JSON writes one byte of a text as at most six, so the files take at
//     most 6 × 64 KiB = 384 KiB of a request.
//   - The inputs take at most 256 KiB.
//   - The largest alert takes under 256 KiB: its evidence at most 16 KiB,
//     and its message, its body, and the names in it under 40 KiB, which
//     JSON writes as at most six times as many.
//
// Together that is under 896 KiB of the 1 MiB.
const (
	ModuleFilesLimit  = 64 * 1024
	ModuleInputsLimit = 256 * 1024
)

// FileInputSuffix ends the name of an input of a module that
// names a file: MMWatchdog reads the file and gives the module its content.
const FileInputSuffix = "_file"

// FindingsLimit is the most findings that one result of a module holds.
const FindingsLimit = 500

// ModuleCall is what a check or a destination states for a module that is
// not core.
type ModuleCall struct {
	// Name is the module's name: the key below which the configuration
	// writes the mapping.
	Name string
	// Inputs are the module's inputs as written.
	Inputs map[string]any
	// ExecOptions say how MMWatchdog runs the module. They are written beside the
	// inputs, and no module has an input with one of their names.
	primitives.ExecOptions
	// Module is the module as this node has it. validate sets it; it is nil
	// for a module that the node does not have, which a joined node can
	// meet in the hub's destinations.
	Module *InstalledModule `json:"-" yaml:"-"`
	// StatedRunSettings is true when the mapping writes run_as_user_account,
	// run_from_directory, or env, rather than leaving them to the defaults.
	StatedRunSettings bool `json:"-" yaml:"-"`
}

// UnmarshalYAML reads the mapping below the module's name: the run settings
// into ExecOptions, and every other entry as an input.
func (m *ModuleCall) UnmarshalYAML(n *yaml.Node) error {
	entries := map[string]any{}
	if n.Kind != yaml.ScalarNode || n.Tag != "!!null" {
		if e := n.Decode(&entries); e != nil {
			return e
		}
	}
	return m.fromEntries(entries, func(value, into any) error {
		b, e := yaml.Marshal(value)
		if e != nil {
			return e
		}
		return yaml.Unmarshal(b, into)
	})
}

// fromEntries splits the entries of the mapping into the run settings and
// the inputs. decode turns one entry into a typed value.
func (m *ModuleCall) fromEntries(entries map[string]any, decode func(value, into any) error) error {
	m.Inputs = map[string]any{}
	for name, value := range entries {
		var e error
		switch name {
		case "run_as_user_account", "run_from_directory", "env":
			m.StatedRunSettings = true
		}
		switch name {
		case "run_as_user_account":
			e = decode(value, &m.RunAsUserAccount)
		case "run_from_directory":
			e = decode(value, &m.RunFromDirectory)
		case "env":
			e = decode(value, &m.Env)
		default:
			m.Inputs[name] = value
		}
		if e != nil {
			return fmt.Errorf("%s: %w", name, e)
		}
	}
	return nil
}

// MarshalJSON writes the mapping as the configuration writes it: the inputs
// with the run settings beside them.
func (m *ModuleCall) MarshalJSON() ([]byte, error) {
	entries := map[string]any{}
	for name, value := range m.Inputs {
		entries[name] = value
	}
	if m.RunAsUserAccount != "" {
		entries["run_as_user_account"] = m.RunAsUserAccount
	}
	if m.RunFromDirectory != "" {
		entries["run_from_directory"] = m.RunFromDirectory
	}
	if len(m.Env) > 0 {
		entries["env"] = m.Env
	}
	return json.Marshal(entries)
}

// UnmarshalJSON reads what MarshalJSON wrote. A number stays as written, so
// a whole number that a float cannot hold reads back as the same number.
func (m *ModuleCall) UnmarshalJSON(b []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	entries := map[string]any{}
	if e := decoder.Decode(&entries); e != nil {
		return e
	}
	return m.fromEntries(entries, func(value, into any) error {
		written, e := json.Marshal(value)
		if e != nil {
			return e
		}
		return json.Unmarshal(written, into)
	})
}

// Validate checks the call against the module as this node has it: the
// module exists and is used in the way of the caller, every input is one
// that the module's description lists and has the type that it states, and
// the run settings are valid. For a destination it also checks what a
// request to the module can hold. A joined node checks the hub's
// destinations without the modules: the node runs a module that it has, and
// a delivery through a module that it does not have fails with that reason.
func (m *ModuleCall) Validate(v InputValidation) *FieldError {
	if !ModuleNamePattern.MatchString(m.Name) {
		return &FieldError{"", "the name of a module uses lower-case letters, digits, and underscores, starts with a letter, and has at most 64 characters"}
	}
	if _, e := json.Marshal(m.Inputs); e != nil {
		return &FieldError{"", "must contain JSON-compatible values: " + e.Error()}
	}
	if problem := ValidateExecOptions(&m.ExecOptions); problem != nil {
		return problem
	}
	if v.UsedAs == UsedAsAlertDestination {
		if written, _ := PlainJSON(ModuleInputs(m.Inputs)); len(written) > ModuleInputsLimit {
			return &FieldError{"", fmt.Sprintf("the inputs are %d bytes as JSON, and the inputs of one destination are at most %d bytes", len(written), ModuleInputsLimit)}
		}
	}
	for _, name := range Names(m.Inputs) {
		if !strings.HasSuffix(name, FileInputSuffix) {
			continue
		}
		if path, ok := m.Inputs[name].(string); !ok || !filepath.IsAbs(path) {
			return &FieldError{name, "an input whose name ends in _file names a file that MMWatchdog reads and gives to the module. Set " + name + " to an absolute file path"}
		}
	}
	if v.WithoutModules {
		m.Module, _ = v.Modules.ModuleIfInstalled(m.Name)
		return nil
	}
	module, e := v.Modules.Module(m.Name)
	if e != nil {
		return &FieldError{"", e.Error()}
	}
	if module.Description.UsedAs != v.UsedAs {
		return &FieldError{"", "the module " + m.Name + " is used as " + usedAsWords(module.Description.UsedAs) + ", not as " + usedAsWords(v.UsedAs) + ": its description says used_as: " + module.Description.UsedAs}
	}
	if problem := module.Description.InputsProblem(m.Name, m.Inputs); problem != nil {
		return problem
	}
	m.Module = module
	if module.IsStarlark() {
		return validateScriptInputs(module, m.Inputs)
	}
	return nil
}

// usedAsWords writes a value of used_as for a message.
func usedAsWords(usedAs string) string {
	if usedAs == UsedAsAlertDestination {
		return "an alert destination"
	}
	return "a check"
}

// ModuleIfInstalled returns the module with that name when this node has it
// and can use it.
func (c *ModuleCatalog) ModuleIfInstalled(name string) (*InstalledModule, bool) {
	if !c.Has(name) {
		return nil, false
	}
	m, e := c.Module(name)
	return m, e == nil
}

// ValidateOnNode checks that the module can start here: an executable can
// be run, a script is Starlark and defines the function that MMWatchdog calls, and
// the module's user account and its directory exist.
func (m *ModuleCall) ValidateOnNode() *FieldError {
	if m.Module == nil {
		return &FieldError{"", "the module " + m.Name + " is not installed on this node"}
	}
	if program := filepath.Join(m.Module.Directory, ModuleExecutableFile); !m.Module.IsStarlark() && !primitives.IsExecutableFile(program) {
		return &FieldError{"", program + " is not an executable file"}
	}
	if m.Module.IsStarlark() {
		if e := checkScript(m.Module.Directory, m.Module.Description.UsedAs == UsedAsAlertDestination); e != nil {
			return &FieldError{"", e.Error()}
		}
	}
	if m.Module.Description.UsedAs == UsedAsCheck {
		if _, problem := m.readCheckFiles(); problem != nil {
			return problem
		}
	}
	return ValidateExecOptionsOnNode(m.ExecOptions)
}

// MeasurementNames lists the measurements that the module's description
// states, for the limits of a check.
func (m *ModuleCall) MeasurementNames() []string {
	if m.Module == nil {
		return nil
	}
	return m.Module.Description.MeasurementNames()
}

// CheckModule is what the engine must know about a check with this module.
func (m *ModuleCall) CheckModule() *CheckModule {
	module := &CheckModule{Name: m.Name, Description: m.description(), LimitsOnItsMeasurements: true}
	if m.Module != nil {
		module.OneIncidentForEachFinding = m.Module.Description.OneIncidentForEachFinding
	}
	return module
}

// destinationModule is what MMWatchdog must know about a destination with this
// module.
func (m *ModuleCall) destinationModule() *destinationModule {
	return &destinationModule{Name: m.Name, Description: m.description()}
}

// description is the module's description, or nil for a module that this
// node does not have.
func (m *ModuleCall) description() *ModuleDescription {
	if m.Module == nil {
		return nil
	}
	return m.Module.Description
}

// kind is the kind of the module, or "" for a module that this node does
// not have.
func (m *ModuleCall) kind() string {
	if m.Module == nil {
		return ""
	}
	return m.Module.Kind
}

// start runs the module once: it writes the request to the module's
// standard input and returns what the module left behind. A script runs
// through this program's run-starlark-module command; an executable runs
// directly, with MMWATCHDOG_MODULE_HELPER in its environment.
func (m *ModuleCall) start(ctx context.Context, request []byte) (primitives.CommandResult, error) {
	if m.Module == nil {
		return primitives.CommandResult{ExitCode: -1}, moduleNotInstalled("the module " + m.Name + " is not installed on this node")
	}
	self, e := os.Executable()
	if e != nil {
		return primitives.CommandResult{ExitCode: -1}, e
	}
	opts := m.ExecOptions
	opts.Env = map[string]string{}
	for name, value := range m.ExecOptions.Env {
		opts.Env[name] = value
	}
	arguments := []string{self, "run-starlark-module", m.Module.Directory}
	if !m.Module.IsStarlark() {
		program := filepath.Join(m.Module.Directory, ModuleExecutableFile)
		if !primitives.IsExecutableFile(program) {
			return primitives.CommandResult{ExitCode: -1}, moduleNotInstalled("the module " + m.Name + " is not installed on this node: " + program + " is not an executable file")
		}
		arguments = []string{program}
		opts.Env["MMWATCHDOG_MODULE_HELPER"] = self
	}
	return primitives.RunCommand(ctx, primitives.Command{Arguments: arguments, StandardInput: request, RunAs: &opts})
}

// mmwatchdogEndsTheModuleAfter is the time that ctx leaves a run of a module, in
// whole milliseconds, for the request to the module: MMWatchdog ends the module at
// the end of ctx. A script ends each call of a primitive shortly before
// then (RunStarlarkModule), so that the script still decides its result. It
// is nil for a context without an end, which only a test gives.
func mmwatchdogEndsTheModuleAfter(ctx context.Context) *int64 {
	deadline, limited := ctx.Deadline()
	if !limited {
		return nil
	}
	left := time.Until(deadline).Milliseconds()
	return &left
}

// moduleNotInstalled is the error of a run of a module that this node does
// not have, or whose program it cannot start.
type moduleNotInstalled string

func (e moduleNotInstalled) Error() string { return string(e) }

// ModuleResult is what a module that a check uses writes to its standard
// output, and what the run_check function of a script returns.
type ModuleResult struct {
	Protocol int    `json:"protocol"`
	Status   string `json:"status"`
	Message  string `json:"message"`
	// Measurements are the numbers that the run measured, by the names
	// that the module's description lists. The description states the unit
	// of each.
	Measurements map[string]float64 `json:"measurements,omitempty"`
	// Findings are the things that the run found to be wrong, for a module
	// whose description states one_incident_for_each_finding.
	Findings []Finding `json:"findings,omitempty"`
}

// checkModuleRequest is what MMWatchdog writes to the standard input of a module
// that a check uses.
type checkModuleRequest struct {
	Protocol int            `json:"protocol"`
	Inputs   map[string]any `json:"inputs"`
	// StateDirectory is the directory that belongs to the check: what the
	// module keeps there is still there at the check's next run.
	StateDirectory string            `json:"state_dir"`
	FileContents   map[string]string `json:"file_contents"`
	// MMWatchdogEndsTheModuleAfterMilliseconds is the time that the run has left
	// when MMWatchdog starts the module (mmwatchdogEndsTheModuleAfter).
	MMWatchdogEndsTheModuleAfterMilliseconds *int64 `json:"mmwatchdog_ends_the_module_after_milliseconds,omitempty"`
}

// readCheckFiles reads the same files for runtime validation and each run.
// The daemon calls it before starting the module under its own account.
func (m *ModuleCall) readCheckFiles() (map[string]string, *FieldError) {
	contents := map[string]string{}
	files := m.CredentialFiles()
	totalFileBytes := 0
	for _, input := range Names(files) {
		content, err := m.ReadCredentialFile(input, files[input])
		if err != nil {
			return contents, &FieldError{input, "MMWatchdog cannot read the file of the input " + input + ": " + err.Error()}
		}
		contents[input] = content
		totalFileBytes += len(content)
	}
	if totalFileBytes > ModuleFilesLimit {
		return contents, &FieldError{"", fmt.Sprintf("the files of the check module hold %d bytes together, and the module request allows at most %d bytes of file contents", totalFileBytes, ModuleFilesLimit)}
	}
	return contents, nil
}

// Run starts the module and reads its result. The module only measures; MMWatchdog
// applies the check's limits to its measurements.
func (m *ModuleCall) Run(ctx context.Context, r *CheckRun) (observation Observation) {
	contents := map[string]string{}
	defer func() {
		observation.Message = WithoutFileContents(observation.Message, contents)
		observation.Stdout = WithoutFileContents(observation.Stdout, contents)
		observation.Stderr = WithoutFileContents(observation.Stderr, contents)
		for index := range observation.Findings {
			observation.Findings[index].ID = WithoutFileContents(observation.Findings[index].ID, contents)
			observation.Findings[index].Message = WithoutFileContents(observation.Findings[index].Message, contents)
			// The words that replace a file's content can be longer than it.
			if exact := observation.Findings[index].ExactFinding; exact != "" {
				observation.Findings[index].ExactFinding = FitExactFinding(WithoutFileContents(exact, contents))
			}
		}
	}()
	var problem *FieldError
	contents, problem = m.readCheckFiles()
	if problem != nil {
		return Result("unknown", "the module "+m.Name+": "+problem.Message)
	}
	// done removes what MMWatchdog made for this run alone, also a copy of the state
	// directory that MMWatchdog could not finish.
	stateDirectory, done, err := r.ModuleStateDirectory(ctx, m.RunAsUserAccount)
	defer done()
	if err != nil {
		return Result("unknown", "MMWatchdog cannot prepare the state directory of the check: "+err.Error())
	}
	request, err := PlainJSON(checkModuleRequest{1, ModuleInputs(m.Inputs), stateDirectory, contents, mmwatchdogEndsTheModuleAfter(ctx)})
	if err != nil {
		return Result("unknown", err.Error())
	}
	ran, err := m.start(ctx, request)
	o := Observation{Protocol: 1, Status: "unknown", Stdout: ran.StandardOutput, Stderr: ran.StandardError}
	if err == nil {
		o.ExitCode = &ran.ExitCode
	}
	if err != nil {
		o.Message = err.Error()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			limit := "its time limit"
			if stated := r.Timing.TimeLimitPerRun.Value(); stated > 0 {
				limit = stated.String() + " (time_limit_per_run)"
			}
			o.Message = fmt.Sprintf("the module %s did not finish within %s, so MMWatchdog stopped the module", m.Name, limit)
		}
		return o
	}
	if ran.ExitCode != 0 {
		o.Message = m.endedWithoutResult(ran.ExitCode, ran.StandardError)
		return o
	}
	var answer ModuleResult
	if err := DecodeJSON([]byte(ran.StandardOutput), &answer); err != nil {
		o.Message = "the module " + m.Name + " wrote a result that is not valid: " + err.Error()
		return o
	}
	if err := ValidateResult(answer); err != nil {
		o.Message = "the module " + m.Name + " wrote a result that is not valid: " + err.Error()
		return o
	}
	description := m.Module.Description
	measurements := map[string]Measurement{}
	for _, name := range Names(answer.Measurements) {
		unit, listed := description.UnitOf(name)
		if !listed {
			o.Message = "the module " + m.Name + " reported the measurement " + name + ", which its description does not list"
			return o
		}
		measurements[name] = Measurement{answer.Measurements[name], unit}
	}
	switch {
	case !description.OneIncidentForEachFinding && len(answer.Findings) > 0:
		o.Message = "the module " + m.Name + " reported findings, and its description does not state one_incident_for_each_finding"
		return o
	case description.OneIncidentForEachFinding && answer.Status == "failed" && len(answer.Findings) == 0:
		o.Message = "the module " + m.Name + " answered failed without a finding, and its description states one_incident_for_each_finding"
		return o
	case answer.Status != "failed" && len(answer.Findings) > 0:
		o.Message = "the module " + m.Name + " reported findings with the status " + answer.Status + ", and only a failed result holds findings"
		return o
	}
	o = Observation{Protocol: 1, Status: answer.Status, Message: answer.Message, Findings: answer.Findings, Stderr: ran.StandardError}
	if len(measurements) > 0 {
		o.Measurements = measurements
	}
	slices.SortFunc(o.Findings, func(a, b Finding) int { return strings.Compare(a.ID, b.ID) })
	switch status, text := applyLimits(r.Check, o, nil, nil); {
	case status == "unknown" && text != "":
		o.Status, o.Message = status, text
	case text != "":
		o.Status, o.Message = status, text+"; the module reported: "+answer.Message
	}
	return o
}

// endedWithoutResult says that a run of the module ended with an exit status
// other than 0, with what the module wrote about that on its standard
// error. An executable writes its diagnostics as it likes, and the first
// line stands for them. The process that runs a script writes the script's
// error as its last line, after everything that the script printed, so that
// line is the error, with the file and the line of the script.
func (m *ModuleCall) endedWithoutResult(exitCode int, standardError string) string {
	lines := strings.Split(strings.TrimSpace(standardError), "\n")
	if m.Module != nil && m.Module.IsStarlark() {
		if last := OneLine(strings.TrimPrefix(lines[len(lines)-1], "mmwatchdog: ")); last != "" {
			return "the script of the module " + m.Name + " ended with an error: " + last
		}
		return fmt.Sprintf("the script of the module %s ended with exit status %d", m.Name, exitCode)
	}
	if first := OneLine(lines[0]); first != "" {
		return fmt.Sprintf("the module %s ended with exit status %d: %s", m.Name, exitCode, first)
	}
	return fmt.Sprintf("the module %s ended with exit status %d", m.Name, exitCode)
}

// ValidateResult checks a result of a module that a check uses, as far as
// the result alone says: its protocol, its status, its message, and the
// form of its measurements and findings. ModuleCall.run checks it against
// the module's description too.
func ValidateResult(r ModuleResult) error {
	if r.Protocol != 1 {
		return fmt.Errorf("unsupported module result protocol %d; expected protocol: 1", r.Protocol)
	}
	if r.Status != "ok" && r.Status != "failed" && r.Status != "unknown" {
		return fmt.Errorf("the check result status must be ok, failed, or unknown")
	}
	if r.Message == "" {
		return fmt.Errorf("a message is required")
	}
	if len(r.Measurements) > 128 {
		return fmt.Errorf("a check result can contain at most 128 measurements")
	}
	for name, value := range r.Measurements {
		if !NamePattern.MatchString(name) {
			return fmt.Errorf("the measurement %q needs a name of 1 to 64 letters, digits, underscores, or hyphens, starting with a letter or digit", name)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("the measurement %q needs a finite number as its value", name)
		}
	}
	if len(r.Findings) > FindingsLimit {
		return fmt.Errorf("a check result can contain at most %d findings", FindingsLimit)
	}
	seen := map[string]bool{}
	for _, finding := range r.Findings {
		if !ValidFindingID(finding.ID) || seen[finding.ID] {
			return fmt.Errorf("each finding needs an id of its own: text on one line of at most %d bytes", FindingIDLimit)
		}
		if finding.Message == "" {
			return fmt.Errorf("the finding %s needs a message", strconv.Quote(finding.ID))
		}
		if !ValidExactFinding(finding.ExactFinding) {
			return fmt.Errorf("the exact_finding of the finding %s must be text on one line of at most %d bytes", strconv.Quote(finding.ID), ExactFindingLimit)
		}
		seen[finding.ID] = true
	}
	return nil
}

func ModuleInputs(inputs map[string]any) map[string]any {
	if inputs == nil {
		return map[string]any{}
	}
	return inputs
}

// The rest of this file is the use of a module as an alert destination.

// sameTarget compares recipient inputs, excluding credential paths and
// presentation inputs. Credentials are compared by SameDestination.
func (m *ModuleCall) sameTarget(other destinationInputs) bool {
	o, same := other.(*ModuleCall)
	if !same {
		return false
	}
	stored := func(call *ModuleCall) string {
		inputs := map[string]any{}
		for name, value := range call.Inputs {
			if strings.HasSuffix(name, FileInputSuffix) {
				continue
			}
			switch name {
			case "mention", "repeat_sos_alerts_every", "stop_repeating_sos_alerts_after":
				continue
			}
			inputs[name] = value
		}
		first, _ := json.Marshal(ModuleInputs(inputs))
		var normalized any
		_ = json.Unmarshal(first, &normalized)
		encoded, _ := json.Marshal(normalized)
		return string(encoded)
	}
	return m.Name == o.Name && stored(m) == stored(o)
}

// CredentialFiles are the files that the inputs name: see FileInputSuffix.
func (m *ModuleCall) CredentialFiles() map[string]string {
	files := map[string]string{}
	for name, value := range m.Inputs {
		if path, ok := value.(string); ok && strings.HasSuffix(name, FileInputSuffix) {
			files[name] = path
		}
	}
	return files
}

// ReadCredentialFile reads a file that an input of a destination module
// names: UTF-8 text of at most ModuleFileLimit bytes, given without the
// line break at its end, which a text editor adds and no credential has.
func (m *ModuleCall) ReadCredentialFile(_, path string) (string, error) {
	b, e := primitives.ReadFile(path, ModuleFileLimit)
	if e != nil && !errors.Is(e, primitives.ErrFileLargerThanLimit) {
		return "", e
	}
	if e != nil || !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return "", fmt.Errorf("%s: expected UTF-8 text of at most %d bytes", path, ModuleFileLimit)
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
}

func (m *ModuleCall) ValidCredential(_, content string) bool {
	return len(content) <= ModuleFileLimit && utf8.ValidString(content) && !strings.ContainsRune(content, 0)
}

func (m *ModuleCall) Sender(delivery DeliveryOf) NotificationSender { return ModuleSender{delivery} }

// destinationModuleRequest is what MMWatchdog writes to a destination module's
// standard input: the alert, the destination's inputs as written, and the
// content of each file that an input names.
type destinationModuleRequest struct {
	Protocol     int               `json:"protocol"`
	Alert        Event             `json:"alert"`
	Inputs       map[string]any    `json:"inputs"`
	FileContents map[string]string `json:"file_contents"`
	// MMWatchdogEndsTheModuleAfterMilliseconds is the time that the delivery
	// attempt has left when MMWatchdog starts the module (mmwatchdogEndsTheModuleAfter).
	MMWatchdogEndsTheModuleAfterMilliseconds *int64 `json:"mmwatchdog_ends_the_module_after_milliseconds,omitempty"`
}

// PlainJSON writes v as JSON with the characters <, >, and & as they are, as
// "module-json object" writes them. A person who reads a module's request
// then sees an input such as "<@U024BE7LH>" as the destination states it.
func PlainJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if e := encoder.Encode(v); e != nil {
		return nil, e
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// destinationModuleResult is what a destination module writes to its
// standard output: delivered or failed, with a message.
type destinationModuleResult struct {
	Protocol int    `json:"protocol"`
	Status   string `json:"status"`
	Message  string `json:"message"`
}

// ModuleSender hands an alert to a destination module, which delivers it to
// its service. One run of the module is one delivery attempt. On a joined
// node the content of the module's files comes from the node's copy of the
// fleet settings; otherwise MMWatchdog reads the files for each delivery.
type ModuleSender struct{ Delivery DeliveryOf }

func (s ModuleSender) Send(ctx context.Context, event Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	destination := s.Delivery.Destination
	m := destination.Inputs.(*ModuleCall)
	contents := s.Delivery.FileContents
	if contents == nil {
		var input string
		var e error
		if contents, input, e = destination.ReadCredentialFiles(); e != nil && input == "" {
			return fmt.Errorf("MMWatchdog cannot use the files of the module %s: %w", m.Name, e)
		} else if e != nil {
			return fmt.Errorf("MMWatchdog cannot read the file of the input %s of the module %s: %w", input, m.Name, e)
		}
	}
	limit := destination.TimeLimitPerDeliveryAttempt.Value()
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	request, e := PlainJSON(destinationModuleRequest{1, event, ModuleInputs(m.Inputs), contents, mmwatchdogEndsTheModuleAfter(ctx)})
	if e != nil {
		return e
	}
	// The limits on the files and on the inputs leave room for the largest
	// alert, so this guards only against an alert that is larger than MMWatchdog
	// writes one today.
	if len(request) > ModuleRequestLimit {
		return fmt.Errorf("the request to the module %s is %d bytes, and a request is at most %d bytes", m.Name, len(request), ModuleRequestLimit)
	}
	// The module is the one that this node has now. A joined node has the
	// hub's destination and its own modules.
	call := *m
	if module, installed := s.Delivery.Modules.ModuleIfInstalled(m.Name); installed {
		call.Module = module
	}
	if call.Module != nil && call.Module.Description.UsedAs != UsedAsAlertDestination {
		return fmt.Errorf("the module %s on this node is used as a check, not as an alert destination", m.Name)
	}
	ran, e := call.start(ctx, request)
	out, diag, code := ran.StandardOutput, ran.StandardError, ran.ExitCode
	// said writes a text that comes from the module on one line, without
	// the content of any file. wrote adds the first line that the module
	// wrote to its standard error, if it wrote one.
	said := func(text string) string { return OneLine(WithoutFileContents(text, contents)) }
	wrote := ""
	if first, _, _ := strings.Cut(strings.TrimSpace(WithoutFileContents(diag, contents)), "\n"); OneLine(first) != "" {
		wrote = ": " + OneLine(first)
	}
	switch {
	case e != nil && parent.Err() != nil:
		return parent.Err()
	case e != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("the module %s did not finish within %s (time_limit_per_delivery_attempt), so MMWatchdog stopped the module", m.Name, limit)
	case errors.Is(e, primitives.ErrTerminatedBySignal):
		return fmt.Errorf("the module %s was ended by a signal%s", m.Name, wrote)
	case errors.Is(e, primitives.ErrStandardOutputLimit):
		return fmt.Errorf("the module %s wrote more than %d bytes to its standard output", m.Name, primitives.ResultLimit)
	case errors.As(e, new(moduleNotInstalled)):
		return e
	case e != nil:
		return fmt.Errorf("the module %s could not run: %s", m.Name, said(e.Error()))
	case code != 0:
		return errors.New(call.endedWithoutResult(code, WithoutFileContents(diag, contents)))
	case strings.TrimSpace(out) == "":
		return fmt.Errorf("the module %s ended without an answer on its standard output%s", m.Name, wrote)
	}
	var result destinationModuleResult
	if e := DecodeJSON([]byte(out), &result); e != nil {
		return fmt.Errorf("the module %s did not answer with one JSON object of protocol, status, and message: %s", m.Name, said(e.Error()))
	}
	switch {
	case result.Protocol != 1:
		return fmt.Errorf("the module %s returned protocol %d; MMWatchdog requires protocol: 1", m.Name, result.Protocol)
	case result.Status != deliveredStatus && result.Status != "failed":
		return fmt.Errorf("the module %s answered with the status %s, and a destination module answers delivered or failed", m.Name, strconv.Quote(said(result.Status)))
	case result.Message == "":
		return fmt.Errorf("the module %s answered %s without a message, and a message is required", m.Name, result.Status)
	case result.Status == "failed":
		return fmt.Errorf("the module %s reported: %s", m.Name, said(result.Message))
	}
	return nil
}

// WithoutFileContents replaces each file content that a text holds whole
// with the name of its input. A module must never repeat a credential in
// its answer; this keeps one out of mmwatchdog status and out of a report to the hub
// if a module does. A module often takes the white space off the ends of a
// content before it uses the content, so the content without that white
// space is replaced too. Even a short secret must not leave the module.
func WithoutFileContents(text string, contents map[string]string) string {
	for _, input := range Names(contents) {
		for _, content := range []string{contents[input], strings.TrimSpace(contents[input])} {
			if content != "" {
				text = strings.ReplaceAll(text, content, "[content of "+input+"]")
				// Invalid module results remain in stdout for diagnosis.
				// JSON escapes must not conceal a credential from redaction.
				for _, encode := range []func(any) ([]byte, error){json.Marshal, PlainJSON} {
					encoded, _ := encode(content)
					text = strings.ReplaceAll(text, string(encoded[1:len(encoded)-1]), "[content of "+input+"]")
				}
			}
		}
	}
	return text
}
