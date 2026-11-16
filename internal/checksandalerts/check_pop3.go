package checksandalerts

import (
	"context"
	"strings"

	"mmwatchdog/internal/primitives"
)

// POP3 is the inputs of the pop3 check module, which talks to a mail server
// that serves mailboxes by POP3: the check waits for the server's greeting,
// switches to encryption when its connection_security says so, and quits.
// It logs in to no mailbox.
type POP3 struct {
	MailServer `yaml:",inline"`
}

func (p *POP3) Run(ctx context.Context, _ *CheckRun) Observation {
	conversation, problem := p.open(ctx)
	if problem != nil {
		return *problem
	}
	defer conversation.close()
	failed := func(step, said string) Observation {
		return Result("failed", primitives.DetailLine("the POP3 server at "+p.Address+": "+step+": "+said))
	}
	// expectOK reads the server's answer, which starts with +OK when the
	// server agrees and with -ERR when it does not.
	expectOK := func(step string) *Observation {
		said, err := conversation.line()
		if err != nil {
			said = err.Error()
		}
		if err != nil || !strings.HasPrefix(said, "+OK") {
			o := failed(step, said)
			return &o
		}
		return nil
	}
	if o := expectOK("its greeting"); o != nil {
		return *o
	}
	answers := "the POP3 server at " + p.Address + " answers"
	if p.ConnectionSecurity != SMTPUnencrypted {
		if p.DangerouslySkipServerCertificateCheck {
			answers += ", with encryption but without checking its certificate"
		} else {
			answers += ", and its certificate is valid"
		}
	}
	if p.ConnectionSecurity == SMTPStartTLS {
		if err := conversation.send("STLS"); err != nil {
			return failed("MMWatchdog's request for encryption", err.Error())
		}
		// STLS is the POP3 command for STARTTLS. A server without
		// encryption, or with encryption from the first byte only, answers
		// -ERR.
		said, err := conversation.line()
		if err != nil {
			return failed("its answer to STLS", err.Error())
		}
		if !strings.HasPrefix(said, "+OK") {
			return Result("failed", primitives.DetailLine("the POP3 server at "+p.Address+" refused to switch to encryption, which connection_security: starttls requires. Its answer to STLS: "+said))
		}
		if err := conversation.startTLS(); err != nil {
			return failed("the switch to encryption", conversation.ended(err).Error())
		}
	}
	if err := conversation.send("QUIT"); err != nil {
		return failed("MMWatchdog's QUIT", err.Error())
	}
	if o := expectOK("its answer to QUIT"); o != nil {
		return *o
	}
	return Result("ok", answers)
}

// pop3Description is what "mmwatchdog describe-module pop3" shows, and what
// validation knows about the module's inputs.
var pop3Description = ModuleDescription{
	Summary: "Talks to a mail server that serves mailboxes, by POP3: waits for the server's greeting and quits. The check logs in to no mailbox.",
	UsedAs:  UsedAsCheck,
	Inputs:  mailServerInputs("POP3"),
	Examples: []ModuleExample{{
		Description:   "The mail server serves mailboxes on its encrypted POP3 port.",
		Configuration: "pop3:\n  address: mail.example.com:995\n  connection_security: implicit_tls\nalert_severity: sos\n",
	}},
}
