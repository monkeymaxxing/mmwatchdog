package checksandalerts

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"syscall"

	"mmwatchdog/internal/primitives"
)

// UnixSocket is the inputs of the unix_socket check module: the absolute
// path of the Unix socket that the check connects to. A Unix socket is a
// file through which programs on the same machine talk to each other. The
// check is healthy when the connection opens.
type UnixSocket string

func (u *UnixSocket) Validate(InputValidation) *FieldError {
	if path := string(*u); !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return &FieldError{"", "must be the absolute path of a Unix socket, such as /run/php/php-fpm.sock"}
	}
	return nil
}

func (u *UnixSocket) Run(ctx context.Context, _ *CheckRun) Observation {
	path := string(*u)
	conn, err := primitives.ConnectUnixSocket(ctx, path)
	switch {
	case err == nil:
		conn.Close()
		return Result("ok", "Unix socket connection accepted")
	case errors.Is(err, fs.ErrNotExist):
		return Result("failed", path+" does not exist")
	case errors.Is(err, syscall.ECONNREFUSED):
		return Result("failed", "no program accepts connections on the Unix socket "+path)
	}
	return Result("failed", err.Error())
}

// unixSocketDescription is what "mmwatchdog describe-module unix_socket" shows, and what
// validation knows about the module's inputs.
var unixSocketDescription = ModuleDescription{
	Summary: "Opens a connection to a Unix socket, a file through which programs on the same machine talk to each other. The check is healthy when the connection opens.",
	UsedAs:  UsedAsCheck,
	Value:   &ModuleInput{Type: "string", Required: true, Description: "The absolute path of the Unix socket."},
	Examples: []ModuleExample{{
		Description:   "The PHP application server accepts a connection on its socket.",
		Configuration: "unix_socket: /run/php/php-fpm.sock\nalert_severity: sos\n",
	}},
}
