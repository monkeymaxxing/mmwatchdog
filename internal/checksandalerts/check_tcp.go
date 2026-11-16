package checksandalerts

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"mmwatchdog/internal/primitives"
)

// TCP is the inputs of the tcp check module. The check connects to Address,
// written as host:port, and is healthy when the connection opens. With
// TextToSend it then sends that text, and with a rule for the reply it reads
// what the server sends until the rule holds.
type TCP struct {
	Address    string `yaml:"address" json:"address"`
	TextToSend string `yaml:"text_to_send" json:"text_to_send,omitempty"`
	ReplyRules `yaml:",inline"`
}

// ReplyRules are the two inputs that say what the reply of a server must
// hold: the tcp and the udp check modules share them. A check can state
// both, and then both must hold.
type ReplyRules struct {
	ReplyContains     string `yaml:"reply_contains" json:"reply_contains,omitempty"`
	ReplyMatchesRegex string `yaml:"reply_matches_regex" json:"reply_matches_regex,omitempty"`
}

// UnmarshalYAML also takes the address alone, written as one string.
func (t *TCP) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		t.Address = n.Value
		return nil
	}
	type plain TCP
	return n.Decode((*plain)(t))
}

func (t *TCP) Validate(v InputValidation) *FieldError {
	if e := HostPort(t.Address, 1); e != nil {
		return &FieldError{"address", e.Error()}
	}
	if v.Authored("text_to_send") && t.TextToSend == "" {
		return &FieldError{"text_to_send", "cannot be empty"}
	}
	return t.ReplyRules.validate(v)
}

func (r *ReplyRules) validate(v InputValidation) *FieldError {
	if v.Authored("reply_contains") && r.ReplyContains == "" {
		return &FieldError{"reply_contains", "cannot be empty"}
	}
	if v.Authored("reply_matches_regex") && r.ReplyMatchesRegex == "" {
		return &FieldError{"reply_matches_regex", "cannot be empty"}
	}
	if _, e := regexp.Compile(r.ReplyMatchesRegex); e != nil {
		return &FieldError{"reply_matches_regex", "is no valid regular expression: " + e.Error()}
	}
	return nil
}

// stated reports whether the check has a rule for the reply.
func (r *ReplyRules) stated() bool { return r.ReplyContains != "" || r.ReplyMatchesRegex != "" }

// hold reports whether a reply keeps every rule, and names the first rule
// that it does not keep.
func (r *ReplyRules) hold(reply string) (holds bool, broken string) {
	if r.ReplyContains != "" && !strings.Contains(reply, r.ReplyContains) {
		return false, "contains " + strconv.Quote(r.ReplyContains)
	}
	if r.ReplyMatchesRegex != "" && !regexp.MustCompile(r.ReplyMatchesRegex).MatchString(reply) {
		return false, "matches the regular expression " + r.ReplyMatchesRegex
	}
	return true, ""
}

// observation is the result of a check from the reply of a server: healthy
// when the reply keeps every rule. A reply that does not is shown in the
// details, so a reader sees what the server said.
func (r *ReplyRules) observation(reply string) Observation {
	holds, broken := r.hold(reply)
	if holds {
		return Result("ok", "the reply holds what the check expects")
	}
	if reply == "" {
		return Result("failed", "the server sent no reply, and the check expects one that "+broken)
	}
	o := Result("failed", "the reply is not one that "+broken)
	o.Details = []string{"the server sent: " + primitives.DetailLine(primitives.Clip(reply, primitives.DetailLineLimit-20))}
	return o
}

// replyObservation requires a received byte for TCP: an empty stream is
// silence, while a UDP packet can carry an empty but real reply.
func (t *TCP) replyObservation(reply string) Observation {
	if reply == "" {
		if holds, _ := t.ReplyRules.hold(reply); holds {
			return Result("failed", "the server sent no reply, and the check expects at least one byte")
		}
	}
	return t.ReplyRules.observation(reply)
}

func (t *TCP) Run(ctx context.Context, r *CheckRun) Observation {
	// Without a rule for the reply, the check reads nothing.
	reply, err := primitives.TCPExchangeUntil(ctx, t.Address, t.TextToSend, func(received []byte) bool {
		holds, _ := t.ReplyRules.hold(string(received))
		return holds && (!t.stated() || len(received) > 0)
	})
	switch {
	case errors.As(err, &primitives.ConnectionDidNotOpen{}) && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return Result("failed", "the connection to "+t.Address+" did not open within "+timeLimitWords(r))
	case errors.As(err, &primitives.ConnectionDidNotOpen{}):
		return Result("failed", err.Error())
	case err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && t.stated():
		// The connection opened, and the reply did not arrive in time.
		o := t.replyObservation(reply)
		o.Message += "; MMWatchdog waited until the time limit of the run"
		return o
	case err != nil:
		return Result("failed", err.Error())
	case !t.stated():
		return Result("ok", "TCP connection accepted")
	}
	return t.replyObservation(reply)
}

// tcpDescription is what "mmwatchdog describe-module tcp" shows, and what
// validation knows about the module's inputs.
var tcpDescription = ModuleDescription{
	Summary: "Opens a TCP connection. The check is healthy when the connection opens. It can also send a text and expect a reply.",
	UsedAs:  UsedAsCheck,
	Value:   &ModuleInput{Type: "string", Description: "The address alone, as host:port, in place of the mapping of inputs."},
	Inputs: []ModuleInput{
		{Name: "address", Type: "string", Required: true, Description: "The address to connect to, as host:port."},
		{Name: "text_to_send", Type: "string", Description: "A text that the check sends once the connection is open. Without it the check sends nothing."},
		{Name: "reply_contains", Type: "string", Description: "A text that the server's reply must contain. Without a rule for the reply, the check reads nothing."},
		{Name: "reply_matches_regex", Type: "string", Description: "A regular expression that the server's reply must match."},
	},
	Examples: []ModuleExample{
		{
			Description:   "The database port of this machine accepts a connection.",
			Configuration: "tcp: 127.0.0.1:5432\nalert_severity: sos\n",
		},
		{
			Description:   "A server of the team's own answers READY to the text STATUS.",
			Configuration: "tcp:\n  address: 127.0.0.1:7000\n  text_to_send: \"STATUS\\r\\n\"\n  reply_contains: READY\nalert_severity: sos\n",
		},
	},
}
