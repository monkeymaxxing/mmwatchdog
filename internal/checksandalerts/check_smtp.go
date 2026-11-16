package checksandalerts

import (
	"context"
	"net"
	"net/smtp"

	"mmwatchdog/internal/primitives"
)

// SMTP is the inputs of the smtp check module, which talks to a mail server
// that accepts mail: the check waits for the server's greeting, greets it
// back, switches to encryption when its connection_security says so, and
// leaves. It sends no mail and logs in nowhere.
type SMTP struct {
	MailServer `yaml:",inline"`
}

func (s *SMTP) Run(ctx context.Context, _ *CheckRun) Observation {
	conversation, problem := s.open(ctx)
	if problem != nil {
		return *problem
	}
	defer conversation.close()
	host, _, _ := net.SplitHostPort(s.Address)
	failed := func(step string, err error) Observation {
		return Result("failed", primitives.DetailLine("the SMTP server at "+s.Address+": "+step+": "+conversation.ended(err).Error()))
	}
	// Go's SMTP client takes each answer of the server into memory, so it
	// reads through the limit of a mail conversation.
	client, err := smtp.NewClient(&primitives.ReadLimitedConnection{Conn: conversation.conn, Remaining: mailConversationLimit}, host)
	if err != nil {
		return failed("its greeting", err)
	}
	if err := client.Hello(smtpHelloName()); err != nil {
		return failed("its answer to MMWatchdog's greeting", err)
	}
	answers := "the SMTP server at " + s.Address + " answers"
	switch s.ConnectionSecurity {
	case SMTPStartTLS:
		if offered, _ := client.Extension("STARTTLS"); !offered {
			return Result("failed", "the SMTP server at "+s.Address+" does not offer STARTTLS, which connection_security: starttls requires")
		}
		if err := client.StartTLS(conversation.secure); err != nil {
			return failed("the switch to encryption", err)
		}
		if s.DangerouslySkipServerCertificateCheck {
			answers += ", with encryption but without checking its certificate"
		} else {
			answers += ", and its certificate is valid"
		}
	case SMTPImplicitTLS:
		if s.DangerouslySkipServerCertificateCheck {
			answers += ", with encryption but without checking its certificate"
		} else {
			answers += ", and its certificate is valid"
		}
	}
	if err := client.Quit(); err != nil {
		return failed("its answer to QUIT", err)
	}
	return Result("ok", answers)
}

// smtpDescription is what "mmwatchdog describe-module smtp" shows, and what
// validation knows about the module's inputs.
var smtpDescription = ModuleDescription{
	Summary: "Talks to a mail server that accepts mail, by SMTP: waits for the server's greeting, greets it back, and leaves. The check sends no mail.",
	UsedAs:  UsedAsCheck,
	Inputs:  mailServerInputs("SMTP"),
	Examples: []ModuleExample{{
		Description:   "The mail server takes a connection on its submission port and switches to encryption.",
		Configuration: "smtp:\n  address: mail.example.com:587\n  connection_security: starttls\nalert_severity: sos\n",
	}},
}
