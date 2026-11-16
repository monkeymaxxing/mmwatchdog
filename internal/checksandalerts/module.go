// Package checksandalerts holds what a check and an alert destination are:
// their settings (settings.go), their modules (one file for each core
// module, check_NAME.go and destination_NAME.go, with the registries in
// checkmodules.go and destinationmodules.go), the modules that are not core
// (Starlark scripts and executables), what one run of a check observes
// (check.go), and the event that a destination delivers, with the text of
// its alert (event.go, alerttext.go). It knows nothing of how a node keeps
// its incidents, stores its state, or joins a fleet: those parts of MMWatchdog use
// this package, and the compiler keeps it from using them.
package checksandalerts

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"go.yaml.in/yaml/v3"
)

// A module is the code that does the work of one check. A check names
// exactly one module, by the module's name, and writes the module's inputs
// below that name. Each core check module has one file, check_NAME.go, that
// holds its input type, the checks of its inputs, and its code; the registry
// in checkmodules.go lists them. The rest of MMWatchdog knows no module by name: it
// asks the registry and the interfaces in this file. The rules are in
// docs/changing-mmwatchdog/design/checks.md under "Modules".

// CheckInputs is what the input type of every check module implements. A
// check holds a pointer to a value of its module's input type.
type CheckInputs interface {
	// Validate fills in the inputs that the check does not state and
	// returns the first input that is wrong. The error's path is the
	// input's path below the module's name, and "" for a module whose value
	// is one scalar, such as tcp.
	Validate(v InputValidation) *FieldError
	// Run observes the check once.
	Run(ctx context.Context, r *CheckRun) Observation
}

// InputValidation is what a module's validate gets beside its inputs.
type InputValidation struct {
	// Authored reports whether the check states the input at path, so that
	// an empty value there is the author's.
	Authored func(path string) bool
	// UsedAs says what states the inputs: UsedAsCheck for a check, and
	// UsedAsAlertDestination for a destination.
	UsedAs string
	// Modules are the modules that are not core on this node.
	Modules *ModuleCatalog
	// WithoutModules is set where a joined node checks the destinations of
	// its hub: a module that the hub names need not be on this node, and
	// its inputs are the hub's to check.
	WithoutModules bool
}

// The interfaces below are optional. A module's input type implements one
// when the engine needs to know that thing about a check with the module.

// ValidatedOnNode is implemented by a module whose check or destination
// needs something on the node that only "mmwatchdog validate" can see,
// such as a credential file or a user account.
type ValidatedOnNode interface {
	ValidateOnNode() *FieldError
}

// NamesCredentialFiles is implemented by a check module whose inputs name
// files that hold a credential, such as a password or a token, which MMWatchdog
// reads for the check. It returns the files by the name of their input. A
// destination module states the same of every destination
// (destinationInputs).
type NamesCredentialFiles interface {
	CredentialFiles() map[string]string
}

// ValidatesWholeCheck is implemented by a module that refuses a setting
// which is written beside its inputs, such as a limit. The error's path is
// below the check.
type ValidatesWholeCheck interface {
	ValidateCheck(c Check) *FieldError
}

// aboutSystemdUnit is implemented by a module whose check is about one
// systemd unit.
type aboutSystemdUnit interface{ SystemdUnitName() string }

// namesMeasurements is implemented by a module that knows, from its inputs,
// which measurements its check reports, in the order its messages name them.
type namesMeasurements interface{ MeasurementNames() []string }

// TakesTimeForEachRun is implemented by a module whose run can take a time
// of its own, such as the sample of a CPU measurement. TimeOfEachRun returns
// that time and what takes it, or 0: the check group's time_limit_per_run
// must be longer.
type TakesTimeForEachRun interface {
	TimeOfEachRun() (time.Duration, string)
}

// RequiresActiveUnit is implemented by a module whose check says that one
// systemd unit must run, and fails for no other reason. A restart of
// that unit is not due through such a check once the unit runs again.
type RequiresActiveUnit interface{ UnitThatMustBeActive() string }

// NormalizedForIdentity is implemented by a module with an input whose
// written form does not change what the check is, such as the order of a
// list. identity returns the inputs as they count for the check's identity.
type NormalizedForIdentity interface{ Identity() CheckInputs }

// CheckRun is what one run of a check gets beside its inputs.
type CheckRun struct {
	// Check is the whole check. A module that measures needs its limits and
	// its alert severity settings to write the observation.
	Check Check
	// Timing is the timing of the check's check group.
	Timing Timing
	// CheckGroup and checkName name the check, and moduleStateDir is the
	// node's module_state_dir: together they say where a module that is
	// not core keeps its state (modulestate.go). diagnostic marks a run of
	// "mmwatchdog check-now", which works on a copy of that state.
	CheckGroup, CheckName, ModuleStateDir string
	Diagnostic                            bool
	// Memory is what the check's module keeps from one run to the next
	// while MMWatchdog runs, such as the last sample of a CPU counter. It does not
	// survive a restart of MMWatchdog, and a run of "mmwatchdog check-now" starts with none.
	Memory *CheckMemory
	// State is what the check's module keeps from one run to the next
	// across restarts of MMWatchdog: see ModuleState.
	State *ModuleState
	// Now is the clock that gives the observation its time: the daemon's,
	// so that the observation's time can feed a wait. Without it, the run
	// takes the wall clock in UTC, as "mmwatchdog check-now --without-daemon" shows.
	Now func() time.Time
}

// CheckMemory holds the value of CheckRun.memory. Only one run of a check
// uses it at a time.
type CheckMemory struct{ Value any }

// ModuleState is the state that a core module keeps for one check between
// runs, as JSON. MMWatchdog stores it with the check's own state, in one commit
// with the observation, and only for a scheduled run that MMWatchdog accepted: a
// run of "mmwatchdog check-now" loads it and changes nothing.
type ModuleState struct {
	Stored json.RawMessage
	// Saved is what the run asked to store, or nil.
	Saved json.RawMessage
}

// load reads the stored state into value, which stays as it is when the
// check has no state yet.
func (s *ModuleState) load(value any) {
	if len(s.Stored) > 0 {
		_ = json.Unmarshal(s.Stored, value)
	}
}

// Save asks MMWatchdog to store value as the check's state.
func (s *ModuleState) Save(value any) {
	if b, e := json.Marshal(value); e == nil {
		s.Saved = b
	}
}

// NewCheckRun is the run of a check that keeps nothing from an earlier run
// and leaves nothing behind for a later one, like a run of "mmwatchdog check-now".
func NewCheckRun(c Check, timing Timing) *CheckRun {
	return &CheckRun{Check: c, Timing: timing, Diagnostic: true, Memory: &CheckMemory{}, State: &ModuleState{}}
}

// CheckModule is one core check module: its name, its input type, and what
// the engine must know about every check with the module.
type CheckModule struct {
	Name string
	// Description says what the module's inputs, measurements, and examples
	// are, for validation and for "mmwatchdog describe-module".
	Description *ModuleDescription
	// Topic is the one of CoreModuleTopics under which the index of the
	// module pages and "mmwatchdog describe-module" list the module.
	Topic string
	// NewInputs returns a pointer to the zero value of the module's input
	// type, for the configuration loader to fill.
	NewInputs func() CheckInputs
	// ScalarStandsFor is, for a module whose inputs are a mapping that can
	// also be written as one scalar, what that scalar must be, such as "a
	// Bash string" for shell. The input type decodes the scalar itself.
	ScalarStandsFor string
	// Measures marks a module whose job is to measure numbers. Its check
	// takes limits on the measurements that its inputs name, and a check
	// without an alert_severity only records its readings.
	Measures bool
	// FailsWithoutALimit marks a module that measures and whose check can
	// also fail without a limit, so it takes an alert_severity without one.
	FailsWithoutALimit bool
	// LimitsOnItsMeasurements marks a module that says "failed" by itself
	// and also reports measurements, such as ping with its round trip time:
	// its check takes limits on the measurements that the module's
	// description lists, and always states an alert_severity. Every module
	// that is not core is one.
	LimitsOnItsMeasurements bool

	// OpensIncidentAtFirstEvent marks inputs that report new events, rather
	// than a condition that must persist before an incident opens.
	OpensIncidentAtFirstEvent func(CheckInputs) bool

	// OneIncidentForEachFinding marks a module whose check reports findings:
	// each finding is its own incident, and the check has no failure
	// incident of its own. findingIsGoneMessage writes the message of the
	// observation in which a finding is no longer reported.
	OneIncidentForEachFinding bool
	findingIsGoneMessage      func(finding string) string
}

// CheckModulesByName and CheckModulesByInputsType index the registry.
var CheckModulesByName, CheckModulesByInputsType = func() (map[string]*CheckModule, map[reflect.Type]*CheckModule) {
	byName, byType := map[string]*CheckModule{}, map[reflect.Type]*CheckModule{}
	for i := range CoreCheckModules {
		m := &CoreCheckModules[i]
		byName[m.Name], byType[reflect.TypeOf(m.NewInputs())] = m, m
	}
	return byName, byType
}()

// CheckModuleNames lists the names of the registry's modules that match, in
// the registry's order, for a message.
func CheckModuleNames(matches func(*CheckModule) bool) []string {
	names := []string{}
	for i := range CoreCheckModules {
		if matches(&CoreCheckModules[i]) {
			names = append(names, CoreCheckModules[i].Name)
		}
	}
	return names
}

// NoCheckModule stands for the module of a check that names none, so that a
// caller can ask it anything without a nil test.
var NoCheckModule = &CheckModule{}

// Module is the check's module, or NoCheckModule for a check without one.
func (c Check) Module() *CheckModule {
	if call, notCore := c.Inputs.(*ModuleCall); notCore {
		return call.CheckModule()
	}
	if m := CheckModulesByInputsType[reflect.TypeOf(c.Inputs)]; m != nil {
		return m
	}
	return NoCheckModule
}

// ModuleKind is the kind of the check's module: ModuleKindCore, or the kind
// of a module that is not core.
func (c Check) ModuleKind() string {
	if call, notCore := c.Inputs.(*ModuleCall); notCore {
		return call.kind()
	}
	return ModuleKindCore
}

// inputsOrNil returns the check's inputs as a plain value, which is nil for
// a check that names no module.
func (c Check) inputsOrNil() any {
	if c.Inputs == nil {
		return nil
	}
	return c.Inputs
}

// ModuleName is the name of the check's module, or "" for a check that
// names none. A check that is written directly on its check group has this
// name.
func (c Check) ModuleName() string { return c.Module().Name }

// ModulesNamedIn returns, for the type of a configuration mapping that names
// a module beside its own settings, the input type of each module that the
// mapping can name, and what the mapping is: a check or a destination. For
// every other type it returns nil.
func ModulesNamedIn(t reflect.Type) (modules map[string]reflect.Type, what string) {
	modules = map[string]reflect.Type{}
	switch t {
	case reflect.TypeOf(CheckGroup{}), reflect.TypeOf(NamedCheck{}):
		for name, m := range CheckModulesByName {
			modules[name] = reflect.TypeOf(m.NewInputs()).Elem()
		}
		return modules, "check"
	case reflect.TypeOf(Destination{}):
		for name, m := range DestinationModulesByName {
			modules[name] = reflect.TypeOf(m.NewInputs()).Elem()
		}
		return modules, "destination"
	}
	return nil, ""
}

// ModuleCallMapping is the type against which validateNode checks the
// mapping below the name of a module that is not core: the three run
// settings, and beside them the module's inputs, which have any name and
// any value that JSON can hold. ModuleCall.validate checks the inputs
// against the module's description.
type ModuleCallMapping map[string]any

// SettingNamesBesideAModule lists every setting that is written beside a
// module's name in a mapping of a check, of a check group, or of a
// destination. No module can have one of these names.
func SettingNamesBesideAModule() []string {
	names := []string{}
	for _, t := range []reflect.Type{reflect.TypeOf(CheckGroup{}), reflect.TypeOf(NamedCheck{}), reflect.TypeOf(Destination{})} {
		names = append(names, Names(Fields(t))...)
	}
	return names
}

// ScalarStandsFor returns what a scalar must be that is written in place of
// the mapping of a module's inputs, or "" for a type that takes no scalar.
func ScalarStandsFor(t reflect.Type) string {
	if m := CheckModulesByInputsType[reflect.PointerTo(t)]; m != nil {
		return m.ScalarStandsFor
	}
	return ""
}

// splitModule separates the entry of the module that a configuration mapping
// names from the mapping's own settings. It returns a mapping without that
// entry, the module's name, and the node of the module's inputs; a mapping
// that names no module returns it whole. own are the mapping's own
// settings. validateNode has refused a mapping with an unknown key or with
// two modules before any mapping is decoded.
func splitModule(n *yaml.Node, own map[string]reflect.Type) (settings *yaml.Node, module string, inputs *yaml.Node) {
	if n.Kind != yaml.MappingNode {
		return n, "", nil
	}
	rest := *n
	rest.Content = nil
	for i := 0; i+1 < len(n.Content); i += 2 {
		if _, isSetting := own[n.Content[i].Value]; !isSetting && module == "" {
			module, inputs = n.Content[i].Value, n.Content[i+1]
			continue
		}
		rest.Content = append(rest.Content, n.Content[i], n.Content[i+1])
	}
	return &rest, module, inputs
}

// decodeInputs fills the check's inputs from the entry of its module. A
// name that no core module has is the name of a module that is not core:
// validation finds it among the node's modules, or refuses the name.
func (c *Check) decodeInputs(module string, inputs *yaml.Node) error {
	if module == "" {
		return nil
	}
	if m := CheckModulesByName[module]; m != nil {
		c.Inputs = m.NewInputs()
		return inputs.Decode(c.Inputs)
	}
	call := &ModuleCall{Name: module}
	c.Inputs = call
	return inputs.Decode(call)
}

func (g *CheckGroup) UnmarshalYAML(n *yaml.Node) error {
	settings, module, inputs := splitModule(n, Fields(reflect.TypeOf(CheckGroup{})))
	type plain CheckGroup
	if e := settings.Decode((*plain)(g)); e != nil {
		return e
	}
	return g.Check.decodeInputs(module, inputs)
}

func (c *NamedCheck) UnmarshalYAML(n *yaml.Node) error {
	settings, module, inputs := splitModule(n, Fields(reflect.TypeOf(NamedCheck{})))
	type plain NamedCheck
	if e := settings.Decode((*plain)(c)); e != nil {
		return e
	}
	return c.Check.decodeInputs(module, inputs)
}

// marshalWithModule writes settings, a struct, as a JSON object and adds the
// entry of a module in front, so that JSON shows a check or a destination
// as the configuration writes it: the module's name with its inputs, beside
// the settings. inputs is nil for a check or a destination that names no
// module.
func marshalWithModule(settings any, module string, inputs any) ([]byte, error) {
	b, e := json.Marshal(settings)
	if e != nil || inputs == nil {
		return b, e
	}
	written, e := json.Marshal(inputs)
	if e != nil {
		return nil, e
	}
	name, _ := json.Marshal(module)
	entry := string(name) + ":" + string(written)
	if string(b) == "{}" {
		return []byte("{" + entry + "}"), nil
	}
	return []byte("{" + entry + "," + string(b[1:])), nil
}

// The three types that hold a check write their JSON with the module's
// entry. Each needs its own method: a method of the embedded Check would
// take the place of the JSON of the whole type and drop its other fields.

func (c Check) MarshalJSON() ([]byte, error) {
	type settings Check
	return marshalWithModule(settings(c), c.ModuleName(), c.inputsOrNil())
}

func (c NamedCheck) MarshalJSON() ([]byte, error) {
	type checkSettings Check
	return marshalWithModule(struct {
		checkSettings
		AlertAfterFailingFor             Duration `json:"alert_after_failing_for,omitempty"`
		RecoveredAfterHealthyFor         Duration `json:"recovered_after_healthy_for,omitempty"`
		AlertAfterFailingForStatedOn     string   `json:"alert_after_failing_for_stated_on,omitempty"`
		RecoveredAfterHealthyForStatedOn string   `json:"recovered_after_healthy_for_stated_on,omitempty"`
	}{checkSettings(c.Check), c.AlertAfterFailingFor, c.RecoveredAfterHealthyFor, c.AlertAfterFailingForStatedOn, c.RecoveredAfterHealthyForStatedOn}, c.ModuleName(), c.inputsOrNil())
}

func (g CheckGroup) MarshalJSON() ([]byte, error) {
	type checkSettings Check
	return marshalWithModule(struct {
		Timing
		Checks             map[string]NamedCheck `json:"checks,omitempty"`
		RestartSystemdUnit *RestartSystemdUnit   `json:"restart_systemd_unit,omitempty"`
		RecoveryCommand    *RecoveryCommand      `json:"recovery_command,omitempty"`
		Heartbeat          *Heartbeat            `json:"heartbeat,omitempty"`
		checkSettings
	}{g.Timing, g.Checks, g.RestartSystemdUnit, g.RecoveryCommand, g.Heartbeat, checkSettings(g.Check)}, g.ModuleName(), g.inputsOrNil())
}
