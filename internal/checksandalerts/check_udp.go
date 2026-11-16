package checksandalerts

import (
	"context"
	"errors"
	"syscall"

	"mmwatchdog/internal/primitives"
)

// UDP is the inputs of the udp check module. The check sends TextToSend in
// one UDP packet to Address, written as host:port, and is healthy when a
// reply arrives that keeps the rules for the reply. UDP has no connection,
// so only a reply says that a program listens.
type UDP struct {
	Address    string `yaml:"address" json:"address"`
	TextToSend string `yaml:"text_to_send" json:"text_to_send"`
	ReplyRules `yaml:",inline"`
}

func (u *UDP) Validate(v InputValidation) *FieldError {
	if e := HostPort(u.Address, 1); e != nil {
		return &FieldError{"address", e.Error()}
	}
	if u.TextToSend == "" {
		return &FieldError{"text_to_send", "is required: the text that the check sends in one UDP packet"}
	}
	return u.ReplyRules.validate(v)
}

func (u *UDP) Run(ctx context.Context, r *CheckRun) Observation {
	reply, err := primitives.UDPExchange(ctx, u.Address, u.TextToSend)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Result("failed", "no reply arrived from "+u.Address+" within "+timeLimitWords(r))
	case errors.Is(err, syscall.ECONNREFUSED):
		return Result("failed", "no program listens on the UDP port of "+u.Address)
	case err != nil:
		return Result("failed", err.Error())
	case !u.stated():
		return Result("ok", "a reply arrived from "+u.Address)
	}
	return u.ReplyRules.observation(reply)
}

// udpDescription is what "mmwatchdog describe-module udp" shows, and what
// validation knows about the module's inputs.
var udpDescription = ModuleDescription{
	Summary: "Sends a text in one UDP packet and is healthy when a reply arrives. It can also expect a text in the reply.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "address", Type: "string", Required: true, Description: "The address to send to, as host:port."},
		{Name: "text_to_send", Type: "string", Required: true, Description: "The text that the check sends in one UDP packet."},
		{Name: "reply_contains", Type: "string", Description: "A text that the reply must contain."},
		{Name: "reply_matches_regex", Type: "string", Description: "A regular expression that the reply must match."},
	},
	Examples: []ModuleExample{{
		Description:   "A server of the team's own answers READY to the text STATUS.",
		Configuration: "udp:\n  address: 127.0.0.1:7001\n  text_to_send: \"STATUS\\n\"\n  reply_contains: READY\nalert_severity: sos\n",
	}},
}
