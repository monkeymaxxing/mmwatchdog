package checksandalerts

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"syscall"

	"mmwatchdog/internal/primitives"
)

// DNS is the inputs of the dns check module, which asks one DNS server for
// the records of one kind for one name. The check is healthy when the
// server answers with at least one record, and with ExpectedRecordValues
// when the answer holds exactly those values.
type DNS struct {
	DNSServer            string   `yaml:"dns_server" json:"dns_server"`
	RecordName           string   `yaml:"record_name" json:"record_name"`
	RecordType           string   `yaml:"record_type" json:"record_type"`
	ExpectedRecordValues []string `yaml:"expected_record_values" json:"expected_record_values,omitempty"`
}

func (d *DNS) Validate(v InputValidation) *FieldError {
	if e := HostPort(d.DNSServer, 1); e != nil {
		return &FieldError{"dns_server", "is required: the DNS server that the check asks, as host:port, such as 10.0.0.2:53"}
	}
	if !slices.Contains(primitives.DNSRecordTypes, d.RecordType) {
		return &FieldError{"record_type", "must be one of " + strings.Join(primitives.DNSRecordTypes, ", ")}
	}
	if d.RecordName == "" {
		return &FieldError{"record_name", "is required: the name that the check asks for, such as www.example.com"}
	}
	for _, character := range []struct{ text, name string }{{" ", "a space"}, {"\t", "a tab"}, {"\r", "a line break"}, {"\n", "a line break"}, {"/", "a slash"}} {
		if strings.Contains(d.RecordName, character.text) {
			return &FieldError{"record_name", "holds " + character.name + ", which a DNS name cannot hold"}
		}
	}
	if length := len(strings.TrimSuffix(d.RecordName, ".")); length > 253 {
		return &FieldError{"record_name", fmt.Sprintf("has %d characters: a DNS name has at most 253 characters", length)}
	}
	if _, isAddress := netip.ParseAddr(d.RecordName); d.RecordType == "PTR" && isAddress != nil {
		return &FieldError{"record_name", "must be an address for record_type: PTR, such as 192.0.2.10: a PTR record gives the name of an address"}
	}
	// The name must be one that a DNS question can hold.
	if _, _, e := primitives.DNSQuestion(0, d.RecordName, d.RecordType); e != nil {
		return &FieldError{"record_name", e.Error()}
	}
	if v.Authored("expected_record_values") && len(d.ExpectedRecordValues) == 0 {
		return &FieldError{"expected_record_values", "cannot be empty: list the values that the answer must hold, or remove expected_record_values"}
	}
	seen := map[string]bool{}
	for _, value := range d.ExpectedRecordValues {
		if value == "" || seen[d.sameDNSValue(value)] {
			return &FieldError{"expected_record_values", "expected distinct values that are not empty"}
		}
		seen[d.sameDNSValue(value)] = true
	}
	return nil
}

// Identity leaves the order in which the expected values are written out of
// the check's identity.
func (d *DNS) Identity() CheckInputs {
	ordered := *d
	ordered.ExpectedRecordValues = slices.Clone(d.ExpectedRecordValues)
	slices.Sort(ordered.ExpectedRecordValues)
	return &ordered
}

// sameDNSValue writes the value of a record so that two ways of writing one
// value are equal: host names in lower case and without a dot at the end,
// and an address in its shortest form. The root name stays ".", the value
// of a null MX record and of an SRV record of a service that is not
// available.
func (d *DNS) sameDNSValue(value string) string {
	switch d.RecordType {
	case "A", "AAAA":
		if address, err := netip.ParseAddr(value); err == nil {
			return address.String()
		}
	case "CNAME", "MX", "NS", "PTR":
		if value != "." {
			return strings.ToLower(strings.TrimSuffix(value, "."))
		}
	case "SRV":
		if host, port, err := net.SplitHostPort(value); err == nil {
			return net.JoinHostPort(strings.ToLower(strings.TrimSuffix(host, ".")), port)
		}
	}
	return value
}

// noServiceAnswer is the message of an answer that says that the domain
// takes no mail, or that the service is not available: an MX record whose
// host is the root name "." (the null MX, RFC 7505), or an SRV record whose
// target is "." (RFC 2782). It is "" for any other answer. values are
// sorted.
func (d *DNS) noServiceAnswer(values []string) string {
	if !slices.Contains(values, ".") {
		return ""
	}
	var answers, record string
	switch d.RecordType {
	case "MX":
		answers, record = d.RecordName+" takes no mail", "is the null MX (.)"
	case "SRV":
		answers, record = "the service "+d.RecordName+" is not available", "names no host (its target is .)"
	default:
		return ""
	}
	message := "the DNS server " + d.DNSServer + " answers that " + answers + ": "
	if len(values) == 1 {
		return primitives.DetailLine(message + "its " + d.RecordType + " record " + record)
	}
	return primitives.DetailLine(message + "one of its " + d.RecordType + " records " + record + "; its " + d.RecordType + " records are " + strings.Join(values, ", "))
}

func (d *DNS) Run(ctx context.Context, _ *CheckRun) Observation {
	asked := d.RecordType + " record of " + d.RecordName + " at the DNS server " + d.DNSServer
	values, err := primitives.DNSLookup(ctx, d.DNSServer, d.RecordName, d.RecordType)
	switch {
	case errors.Is(err, primitives.ErrNoDNSRecord):
		return Result("failed", "the DNS server "+d.DNSServer+" has no "+d.RecordType+" record of "+d.RecordName)
	case err != nil && (ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded)):
		return Result("failed", "the DNS server "+d.DNSServer+" did not answer within the time limit of the run")
	case errors.Is(err, syscall.ECONNREFUSED):
		return Result("failed", "no DNS server listens at "+d.DNSServer)
	case err != nil:
		return Result("failed", primitives.DetailLine("MMWatchdog got no "+asked+": "+err.Error()))
	}
	slices.Sort(values)
	answer := primitives.DetailLine("the " + asked + " is " + strings.Join(values, ", "))
	if len(d.ExpectedRecordValues) == 0 {
		// A check without expected values asks where the domain's mail or
		// service goes. An answer of nowhere fails it; a check of a domain
		// that must take no mail expects the value "." instead.
		if refusal := d.noServiceAnswer(values); refusal != "" {
			return Result("failed", refusal)
		}
		return Result("ok", answer)
	}
	got, want := []string{}, []string{}
	for _, value := range values {
		got = append(got, d.sameDNSValue(value))
	}
	for _, value := range d.ExpectedRecordValues {
		want = append(want, d.sameDNSValue(value))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(slices.Compact(got), want) {
		expected := slices.Clone(d.ExpectedRecordValues)
		slices.Sort(expected)
		return Result("failed", primitives.DetailLine(answer+", expected "+strings.Join(expected, ", ")))
	}
	return Result("ok", answer)
}

// dnsDescription is what "mmwatchdog describe-module dns" shows, and what
// validation knows about the module's inputs.
var dnsDescription = ModuleDescription{
	Summary: "Asks one DNS server for the records of one kind for one name. The check is healthy when the server answers with a record, and can also compare the answer with the values that are expected.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "dns_server", Type: "string", Required: true, Description: "The DNS server that the check asks, as host:port, such as 10.0.0.2:53."},
		{Name: "record_name", Type: "string", Required: true, Description: "The name that the check asks for, such as www.example.com. For record_type PTR, an address."},
		{Name: "record_type", Type: "string", Required: true, AllowedValues: func() []any {
			types := []any{}
			for _, recordType := range primitives.DNSRecordTypes {
				types = append(types, recordType)
			}
			return types
		}(), Description: "The kind of record to ask for."},
		{Name: "expected_record_values", Type: "list_of_strings", Description: "The values that the answer must hold, all of them and no other, in any order: addresses for A and AAAA, host names for CNAME, MX, NS, and PTR, texts for TXT, and host:port for SRV. A domain that must take no mail, or a service that must not be offered, expects \".\", the null MX or the SRV record without a host."},
	},
	Examples: []ModuleExample{{
		Description:   "The internal DNS server gives the address of the application.",
		Configuration: "dns:\n  dns_server: 10.0.0.2:53\n  record_name: app.internal.example.com\n  record_type: A\n  expected_record_values: [10.0.0.5]\nalert_severity: sos\n",
	}},
}
