package checksandalerts

import (
	"bufio"
	"context"
	"io"
	"strings"

	"mmwatchdog/internal/primitives"
)

// SSH is the inputs of the ssh check module: the address of an SSH server,
// written as host:port. The check asks the server for its greeting, the
// line with which every SSH server starts, and logs in nowhere.
type SSH string

func (s *SSH) Validate(InputValidation) *FieldError {
	if e := HostPort(string(*s), 1); e != nil {
		return &FieldError{"", e.Error()}
	}
	return nil
}

// sshGreetingLimit is the most that MMWatchdog reads while it waits for the
// greeting of an SSH server. A server may send other lines first.
const sshGreetingLimit = 16 * 1024

func (s *SSH) Run(ctx context.Context, _ *CheckRun) Observation {
	address := string(*s)
	conn, err := primitives.ConnectTCP(ctx, address)
	if err != nil {
		return Result("failed", connectionError(ctx, address, err))
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	lines, sent := bufio.NewReader(io.LimitReader(conn, sshGreetingLimit)), []string{}
	for {
		line, err := lines.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "SSH-") {
			// A client that leaves without a word is an error in the
			// server's log. MMWatchdog names itself as a client does, and leaves.
			_, _ = io.WriteString(conn, "SSH-2.0-MMWatchdog\r\n")
			return Result("ok", primitives.DetailLine("the SSH server at "+address+" greeted with "+line))
		}
		if line != "" {
			sent = append(sent, line)
		}
		if err != nil {
			// A server of another protocol often greets in its own way and
			// then waits for the client, until the time limit ends the run.
			o := Result("failed", "the server at "+address+" sent no SSH greeting")
			if ctx.Err() != nil {
				o.Message += " within the time limit of the run"
			}
			if len(sent) > 0 {
				o.Details = []string{"the server sent: " + primitives.DetailLine(primitives.Clip(strings.Join(sent, " | "), primitives.DetailLineLimit-20))}
			}
			return o
		}
	}
}

// sshDescription is what "mmwatchdog describe-module ssh" shows, and what
// validation knows about the module's inputs.
var sshDescription = ModuleDescription{
	Summary: "Asks an SSH server for its greeting, the line with which every SSH server starts. The check logs in nowhere.",
	UsedAs:  UsedAsCheck,
	Value:   &ModuleInput{Type: "string", Required: true, Description: "The address of the SSH server, as host:port."},
	Examples: []ModuleExample{{
		Description:   "The SSH server of this machine answers.",
		Configuration: "ssh: 127.0.0.1:22\nalert_severity: sos\n",
	}},
}
