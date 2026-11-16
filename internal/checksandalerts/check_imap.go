package checksandalerts

import (
	"context"
	"strings"

	"mmwatchdog/internal/primitives"
)

// IMAP is the inputs of the imap check module, which talks to a mail server
// that serves mailboxes: the check waits for the server's greeting,
// switches to encryption when its connection_security says so, and logs
// out. It logs in to no mailbox.
type IMAP struct {
	MailServer `yaml:",inline"`
}

func (i *IMAP) Run(ctx context.Context, _ *CheckRun) Observation {
	conversation, problem := i.open(ctx)
	if problem != nil {
		return *problem
	}
	defer conversation.close()
	failed := func(step, said string) Observation {
		return Result("failed", primitives.DetailLine("the IMAP server at "+i.Address+": "+step+": "+said))
	}
	// A server that is ready greets with "* OK", and one that takes the
	// client for logged in already with "* PREAUTH".
	greeting, err := conversation.line()
	if err != nil {
		return failed("its greeting", err.Error())
	}
	if !strings.HasPrefix(greeting, "* OK") && !strings.HasPrefix(greeting, "* PREAUTH") {
		return failed("its greeting", "expected a line that starts with * OK, received "+greeting)
	}
	answers := "the IMAP server at " + i.Address + " answers"
	if i.ConnectionSecurity != SMTPUnencrypted {
		if i.DangerouslySkipServerCertificateCheck {
			answers += ", with encryption but without checking its certificate"
		} else {
			answers += ", and its certificate is valid"
		}
	}
	if i.ConnectionSecurity == SMTPStartTLS {
		if said, err := conversation.command("m1", "STARTTLS"); err != nil {
			return failed("its answer to STARTTLS", err.Error())
		} else if !strings.HasPrefix(said, "m1 OK") {
			// A server without encryption, or with encryption from the
			// first byte only, answers NO or BAD.
			return Result("failed", primitives.DetailLine("the IMAP server at "+i.Address+" refused to switch to encryption, which connection_security: starttls requires. Its answer to STARTTLS: "+strings.TrimPrefix(said, "m1 ")))
		}
		if err := conversation.startTLS(); err != nil {
			return failed("the switch to encryption", conversation.ended(err).Error())
		}
	}
	if said, err := conversation.command("m2", "LOGOUT"); err != nil {
		return failed("its answer to LOGOUT", err.Error())
	} else if !strings.HasPrefix(said, "m2 OK") {
		return failed("its answer to LOGOUT", said)
	}
	return Result("ok", answers)
}

// command sends one IMAP command under a tag and returns the line with
// which the server ends its answer: the one that starts with the tag. The
// server can send other lines first, each with a star in front.
func (c *mailConversation) command(tag, command string) (string, error) {
	if err := c.send(tag + " " + command); err != nil {
		return "", err
	}
	for {
		line, err := c.line()
		if err != nil || strings.HasPrefix(line, tag+" ") {
			return line, err
		}
	}
}

// imapDescription is what "mmwatchdog describe-module imap" shows, and what
// validation knows about the module's inputs.
var imapDescription = ModuleDescription{
	Summary: "Talks to a mail server that serves mailboxes, by IMAP: waits for the server's greeting and logs out. The check logs in to no mailbox.",
	UsedAs:  UsedAsCheck,
	Inputs:  mailServerInputs("IMAP"),
	Examples: []ModuleExample{{
		Description:   "The mail server serves mailboxes on its encrypted IMAP port.",
		Configuration: "imap:\n  address: mail.example.com:993\n  connection_security: implicit_tls\nalert_severity: sos\n",
	}},
}
