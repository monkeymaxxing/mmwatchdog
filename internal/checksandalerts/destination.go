package checksandalerts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"

	"go.yaml.in/yaml/v3"
)

// An alert destination names exactly one module, as a check does: the code
// that delivers an alert to one place. Each core destination module has one
// file, destination_NAME.go, that holds its input type, the checks of its
// inputs, and its sender; the registry in destinationmodules.go lists them.
// The rest of MMWatchdog knows no destination module by name, except the connection
// to MMHotline, which is a part of MMWatchdog of its own (mmhotline.go). The rules
// are in docs/changing-mmwatchdog/design/alerts.md under "Destinations".

// destinationInputs is what the input type of every destination module
// implements. A destination holds a pointer to a value of its module's
// input type.
type destinationInputs interface {
	// Validate fills in the inputs that the destination does not state and
	// returns the first input that is wrong, with its path below the
	// module's name.
	Validate(v InputValidation) *FieldError
	// sameTarget reports whether other, the inputs of a destination with
	// the same module, deliver to the same receiving address. SameDestination compares
	// credential contents separately. It decides whether a queued delivery still belongs to a
	// destination after a reload.
	sameTarget(other destinationInputs) bool
	// CredentialFiles are the files that hold the destination's secrets, by
	// the name of the input that names each. MMWatchdog reads them for each
	// delivery, and a hub sends their contents to its joined nodes.
	CredentialFiles() map[string]string
	// ReadCredentialFile reads the file of one input and returns the secret
	// in it, as text.
	ReadCredentialFile(input, path string) (string, error)
	// ValidCredential reports a text that ReadCredentialFile can return for
	// the input, for a joined node that checks its copy of the hub's
	// settings.
	ValidCredential(input, content string) bool
	// Sender returns what delivers one alert to the destination.
	Sender(delivery DeliveryOf) NotificationSender
}

// ValidatesWholeDestination is implemented by a module that refuses a
// setting that every other destination takes. The error's path is below
// the destination.
type ValidatesWholeDestination interface {
	ValidateDestination(d Destination) *FieldError
}

// DeliveryOf is what a destination module gets to build the sender of one
// delivery.
type DeliveryOf struct {
	// Destination is the destination as the active configuration has it.
	Destination Destination
	// SendOnlyTheAlertTitle is the destination's send_only_the_alert_title
	// when the alert was queued.
	SendOnlyTheAlertTitle bool
	// FileContents holds, on a joined node, the content of each credential
	// file of the destination from the node's copy of the fleet settings,
	// by the name of its input: a joined node never reads the hub's paths
	// on its own disk. nil reads the files for each delivery.
	FileContents map[string]string
	// Modules are the modules that are not core on this node.
	Modules *ModuleCatalog
	// MMHotline is this destination's connection state, shared by its
	// alert deliveries and check-ins.
	MMHotline *MMHotlineConnection
}

// Credential returns the secret of one input: from fileContents on a joined
// node, and otherwise read from the input's file now.
func (d DeliveryOf) Credential(input string) (string, error) {
	if d.FileContents != nil {
		return d.FileContents[input], nil
	}
	return d.Destination.ReadCredentialFile(input)
}

// destinationModule is one core destination module: its name, its input
// type, and what MMWatchdog must know about every destination with the module.
type destinationModule struct {
	Name string
	// Description says what the module's inputs and examples are, for
	// validation and for "mmwatchdog describe-module".
	Description *ModuleDescription
	// Topic is the one of CoreModuleTopics under which the index of the
	// module pages and "mmwatchdog describe-module" list the module.
	Topic string
	// NewInputs returns a pointer to the zero value of the module's input
	// type, for the configuration loader to fill.
	NewInputs func() destinationInputs
	// DefaultTimeLimit is the limit of one delivery attempt when the
	// destination states none; "" is the general default of 5s.
	DefaultTimeLimit Duration
}

// DestinationModulesByName and DestinationModulesByInputsType index the
// registry.
var DestinationModulesByName, DestinationModulesByInputsType = func() (map[string]*destinationModule, map[reflect.Type]*destinationModule) {
	byName, byType := map[string]*destinationModule{}, map[reflect.Type]*destinationModule{}
	for i := range CoreDestinationModules {
		m := &CoreDestinationModules[i]
		byName[m.Name], byType[reflect.TypeOf(m.NewInputs())] = m, m
	}
	return byName, byType
}()

// DestinationModuleNames lists the names of the registry's modules, in the
// registry's order, for a message.
func DestinationModuleNames() []string {
	names := []string{}
	for _, m := range CoreDestinationModules {
		names = append(names, m.Name)
	}
	return names
}

// noDestinationModule stands for the module of a destination that names
// none, so that a caller can ask it anything without a nil test.
var noDestinationModule = &destinationModule{}

// Module is the destination's module, or noDestinationModule for a
// destination without one, such as the hub's queue on a joined node.
func (d Destination) Module() *destinationModule {
	if call, notCore := d.Inputs.(*ModuleCall); notCore {
		return call.destinationModule()
	}
	if m := DestinationModulesByInputsType[reflect.TypeOf(d.Inputs)]; m != nil {
		return m
	}
	return noDestinationModule
}

// ModuleKind is the kind of the destination's module: ModuleKindCore, or
// the kind of a module that is not core.
func (d Destination) ModuleKind() string {
	if call, notCore := d.Inputs.(*ModuleCall); notCore {
		return call.kind()
	}
	return ModuleKindCore
}

// ModuleName is the name of the destination's module, or "".
func (d Destination) ModuleName() string { return d.Module().Name }

// ServiceKeepsIncidents reports a destination whose module's description
// states service_keeps_incidents: true: its service opens an incident of
// its own for a failure and closes it for the alert that closes the
// incident, so MMWatchdog keeps a fleet incident set for the destination
// (store.IncidentSetOf). A module that this node does not have keeps none.
func (d Destination) ServiceKeepsIncidents() bool {
	description := d.Module().Description
	return description != nil && description.ServiceKeepsIncidents
}

// PathOfInput is the path, below the destination, of one input of its
// module, or of the module's mapping for "".
func (d Destination) PathOfInput(input string) string {
	if input == "" {
		return d.ModuleName()
	}
	return d.ModuleName() + "." + input
}

// credentialFilesLimit is the most bytes that the credential files of one
// destination hold together.
const credentialFilesLimit = ModuleFilesLimit

// ReadCredentialFiles reads every credential file of the destination and
// returns the contents by the name of the input. With an error it returns
// the name of the input whose file it could not read, or "" when the files
// hold too much together.
func (d Destination) ReadCredentialFiles() (contents map[string]string, input string, e error) {
	contents = map[string]string{}
	if d.Inputs == nil {
		return contents, "", nil
	}
	for _, input := range Names(d.Inputs.CredentialFiles()) {
		content, e := d.ReadCredentialFile(input)
		if e != nil {
			return nil, input, e
		}
		contents[input] = content
	}
	if e := d.CredentialFilesTooLarge(contents); e != nil {
		return nil, "", e
	}
	return contents, "", nil
}

// CredentialFilesTooLarge bounds secrets together and public certificate
// authorities separately, as their readers do.
func (d Destination) CredentialFilesTooLarge(contents map[string]string) error {
	together := 0
	for input, content := range contents {
		_, external := d.Inputs.(*ModuleCall)
		if !external && input == "trusted_certificate_authority_file" {
			if len(content) > certificateAuthorityFileLimit {
				return fmt.Errorf("the certificate authority file holds %d bytes, and its limit is %d bytes", len(content), certificateAuthorityFileLimit)
			}
			continue
		}
		together += len(content)
	}
	if together > credentialFilesLimit {
		return fmt.Errorf("the files named in the destination's inputs total %d bytes, more than the limit of %d bytes for one destination", together, credentialFilesLimit)
	}
	return nil
}

// CaptureCredentialContent fingerprints the credential contents at activation.
// Unreadable credentials have no fingerprint and cannot cancel queued alerts.
// fileContents is supplied by the fleet hub on a joined node. Certificate
// authorities, MMHotline signing secrets, the core webhook module's bearer
// token and the core email module's login password can rotate without
// changing the identity of queued alerts: they prove who sends, while the
// receiver is the URL or the mail server and recipients, which still count.
// Credentials of other modules stay in the fingerprint, because their files
// can name the receiver (a Pushover user key, a Slack webhook URL).
func (d *Destination) CaptureCredentialContent(fileContents map[string]string) {
	contents := map[string]string{}
	if d.Inputs != nil {
		for input, path := range d.Inputs.CredentialFiles() {
			if IsCertificateAuthorityFileInput(input) || d.credentialAuthenticatesTheSender(input) {
				continue
			}
			content, err := "", error(nil)
			if fileContents != nil {
				content = fileContents[input]
			} else {
				content, err = d.Inputs.ReadCredentialFile(input, path)
			}
			if err != nil {
				d.CredentialContentFingerprint = ""
				return
			}
			contents[input] = content
		}
	}
	encoded, _ := json.Marshal(contents)
	d.CredentialContentFingerprint = fmt.Sprintf("%x", sha256.Sum256(encoded))
}

// credentialAuthenticatesTheSender reports a credential file of a core module
// that proves who sends and never names who receives.
func (d Destination) credentialAuthenticatesTheSender(input string) bool {
	switch d.Inputs.(type) {
	case *MMHotlineDestination:
		return input == "mmhotline_signing_secret_file"
	case *Webhook:
		return input == "bearer_token_file"
	case *Email:
		return input == "login_password_file"
	}
	return false
}

// SameDestination compares the receiving address and captured credential
// contents. File paths and message formatting cannot discard queued alerts.
func SameDestination(a, b Destination) bool {
	if a.Inputs == nil || b.Inputs == nil {
		return a.Inputs == nil && b.Inputs == nil
	}
	if a.ModuleName() != b.ModuleName() {
		return false
	}
	if a.CredentialContentFingerprint != "" && b.CredentialContentFingerprint != "" && a.CredentialContentFingerprint != b.CredentialContentFingerprint {
		return false
	}
	return a.Inputs.sameTarget(b.Inputs)
}

func (d *Destination) UnmarshalYAML(n *yaml.Node) error {
	settings, module, inputs := splitModule(n, Fields(reflect.TypeOf(Destination{})))
	type plain Destination
	if e := settings.Decode((*plain)(d)); e != nil {
		return e
	}
	if module == "" {
		return nil
	}
	if m := DestinationModulesByName[module]; m != nil {
		d.Inputs = m.NewInputs()
		return inputs.Decode(d.Inputs)
	}
	// A name that no core module has is the name of a module that is not
	// core: validation finds it among the node's modules, or refuses it.
	call := &ModuleCall{Name: module}
	d.Inputs = call
	return inputs.Decode(call)
}

// MarshalJSON writes the destination as the configuration writes it: the
// module's name with its inputs, beside the destination's settings.
func (d Destination) MarshalJSON() ([]byte, error) {
	type settings Destination
	if d.Inputs == nil {
		return marshalWithModule(settings(d), "", nil)
	}
	return marshalWithModule(settings(d), d.ModuleName(), d.Inputs)
}

// UnmarshalJSON reads what MarshalJSON wrote: a queued delivery keeps its
// destination, and a joined node keeps the hub's destinations. It refuses a
// member that is neither a setting of a destination nor a module, and a
// second module. A number in a module's inputs stays as it is written, so a
// whole number beyond 2^53 reads back as the same number and the version of
// the fleet settings stays the same on a joined node.
func (d *Destination) UnmarshalJSON(b []byte) error {
	var entries map[string]json.RawMessage
	if e := json.Unmarshal(b, &entries); e != nil {
		return e
	}
	type settings Destination
	var read settings
	own, isSetting := map[string]json.RawMessage{}, Fields(reflect.TypeOf(Destination{}))
	for _, name := range Names(entries) {
		if _, setting := isSetting[name]; setting {
			own[name] = entries[name]
			continue
		}
		if read.Inputs != nil {
			return fmt.Errorf("a destination names exactly one module; remove the second module, %s", name)
		}
		// A name that is no setting is the destination's module: a core
		// one, or one that is not core, which the hub of a joined node can
		// name without the node having it.
		if m := DestinationModulesByName[name]; m != nil {
			read.Inputs = m.NewInputs()
		} else if ModuleNamePattern.MatchString(name) {
			read.Inputs = &ModuleCall{Name: name}
		} else {
			return fmt.Errorf("unknown field %q", name)
		}
		inputs := json.NewDecoder(bytes.NewReader(entries[name]))
		inputs.UseNumber()
		inputs.DisallowUnknownFields()
		if e := inputs.Decode(read.Inputs); e != nil {
			return e
		}
	}
	written, e := json.Marshal(own)
	if e != nil {
		return e
	}
	rest := json.NewDecoder(bytes.NewReader(written))
	rest.DisallowUnknownFields()
	if e := rest.Decode(&read); e != nil {
		return e
	}
	*d = Destination(read)
	return nil
}

// NotificationSender owns formatting and transport; the outbox owns retries,
// ordering, event identity and durable acknowledgment. Send returns nil only
// after the destination has acknowledged the event, and must honor ctx.
type NotificationSender interface {
	Send(context.Context, Event) error
}

// ReadCredentialFile reads one destination input without enforcing file
// permissions. Only validation and reload refuse a permission problem:
// changing a mode on a running node must not drop an alert.
func (d Destination) ReadCredentialFile(input string) (string, error) {
	return d.Inputs.ReadCredentialFile(input, d.Inputs.CredentialFiles()[input])
}
