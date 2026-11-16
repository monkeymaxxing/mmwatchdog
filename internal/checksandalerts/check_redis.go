package checksandalerts

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"mmwatchdog/internal/primitives"
)

// Redis is the inputs of the redis check module, which asks a Redis server
// for an answer: the check sends PING and expects PONG. With LoginPasswordFile it
// logs in first, as the account in LoginUser when the server has accounts.
type Redis struct {
	DangerouslySkipServerCertificateCheck bool   `yaml:"dangerously_skip_server_certificate_check" json:"dangerously_skip_server_certificate_check,omitempty"`
	ConnectionSecurity                    string `yaml:"connection_security" json:"connection_security"`
	TrustedCertificateAuthorityFile       string `yaml:"trusted_certificate_authority_file" json:"trusted_certificate_authority_file,omitempty"`
	Address                               string `yaml:"address" json:"address"`
	LoginUser                             string `yaml:"login_user" json:"login_user,omitempty"`
	LoginPasswordFile                     string `yaml:"login_password_file" json:"login_password_file,omitempty"`
}

func (r *Redis) Validate(InputValidation) *FieldError {
	if e := HostPort(r.Address, 1); e != nil {
		return &FieldError{"address", e.Error()}
	}
	switch {
	case r.ConnectionSecurity != databaseVerifiedTLS && r.ConnectionSecurity != databaseUnencrypted:
		return &FieldError{"connection_security", "is required: write connection_security: verified_tls to encrypt the connection, or connection_security: unencrypted to send without encryption"}
	case r.TrustedCertificateAuthorityFile != "" && !filepath.IsAbs(r.TrustedCertificateAuthorityFile):
		return &FieldError{"trusted_certificate_authority_file", "must be absolute: write the full path of the trusted certificate authorities"}
	case r.TrustedCertificateAuthorityFile != "" && r.ConnectionSecurity != databaseVerifiedTLS:
		return &FieldError{"trusted_certificate_authority_file", "has no use without TLS: change connection_security to verified_tls or remove trusted_certificate_authority_file"}
	case r.LoginUser != "" && r.LoginPasswordFile == "":
		return &FieldError{"login_password_file", "required with login_user: the file that holds the password of " + r.LoginUser}
	case !PlainText(r.LoginUser, 256) || strings.ContainsAny(r.LoginUser, " "):
		return &FieldError{"login_user", "expected one word of at most 256 bytes"}
	case r.LoginPasswordFile != "" && !filepath.IsAbs(r.LoginPasswordFile):
		return &FieldError{"login_password_file", "must be absolute"}
	}
	return nil
}

// CredentialFiles is the file that holds the password of the login.
func (r *Redis) CredentialFiles() map[string]string {
	if r.LoginPasswordFile == "" {
		return nil
	}
	return map[string]string{"login_password_file": r.LoginPasswordFile}
}

// ValidateOnNode checks that the password file can be read here. The check
// reads it again for each run.
func (r *Redis) ValidateOnNode() *FieldError {
	if r.LoginPasswordFile != "" {
		if _, err := ReadPassword(r.LoginPasswordFile); err != nil {
			return &FieldError{"login_password_file", err.Error()}
		}
	}
	if r.TrustedCertificateAuthorityFile != "" {
		if _, err := readTrustedCertificateAuthorityFile(r.TrustedCertificateAuthorityFile); err != nil {
			return &FieldError{"trusted_certificate_authority_file", err.Error()}
		}
	}
	return nil
}

// RedisCommand writes one command as Redis reads it: the number of its
// words, and each word with its length in front, so a password can hold any
// character.
func RedisCommand(words ...string) string {
	command := "*" + strconv.Itoa(len(words)) + "\r\n"
	for _, word := range words {
		command += "$" + strconv.Itoa(len(word)) + "\r\n" + word + "\r\n"
	}
	return command
}

func (r *Redis) Run(ctx context.Context, _ *CheckRun) Observation {
	var secure *tls.Config
	if r.ConnectionSecurity == databaseVerifiedTLS {
		secure = primitives.ServerTLSConfig(nil, r.DangerouslySkipServerCertificateCheck)
		secure.ServerName, _, _ = net.SplitHostPort(r.Address)
		secure.MinVersion = tls.VersionTLS12
		if r.TrustedCertificateAuthorityFile != "" {
			roots, err := readTrustedCertificateAuthorityFile(r.TrustedCertificateAuthorityFile)
			if err != nil {
				return Result("unknown", err.Error())
			}
			secure.RootCAs = roots
		}
	}
	commands, answers, password := "", 1, ""
	if r.LoginPasswordFile != "" {
		var err error
		if password, err = ReadPassword(r.LoginPasswordFile); err != nil {
			return Result("unknown", err.Error())
		}
		login := []string{"AUTH", password}
		if r.LoginUser != "" {
			login = []string{"AUTH", r.LoginUser, password}
		}
		commands, answers = RedisCommand(login...), 2
	}
	// The server answers each command with one line: +OK and +PONG, or a
	// line that starts with a minus sign and says what is wrong.
	reply, err := primitives.TCPExchangeUntilWithTLS(ctx, r.Address, secure, commands+RedisCommand("PING"), func(received []byte) bool {
		return bytes.Count(received, []byte("\r\n")) >= answers
	})
	// A server that is no Redis server can repeat what it was sent, and
	// MMWatchdog shows what a server sent: never with the password in it.
	if password != "" {
		reply = strings.ReplaceAll(reply, password, "(the password)")
	}
	if o := serverThatDidNotAnswer(ctx, r.Address, reply, err); o != nil {
		return *o
	}
	lines := strings.Split(strings.TrimSuffix(reply, "\r\n"), "\r\n")
	for _, line := range lines {
		if problem, isError := strings.CutPrefix(line, "-"); isError {
			return Result("failed", primitives.DetailLine("the Redis server at "+r.Address+" answered: "+problem))
		}
	}
	if len(lines) != answers || lines[answers-1] != "+PONG" {
		o := Result("failed", "the server at "+r.Address+" did not answer PING as a Redis server does")
		if reply != "" {
			o.Details = []string{"the server sent: " + primitives.DetailLine(primitives.Clip(reply, primitives.DetailLineLimit-20))}
		}
		return o
	}
	return Result("ok", "the Redis server at "+r.Address+" answered PING")
}

// redisDescription is what "mmwatchdog describe-module redis" shows, and what
// validation knows about the module's inputs.
var redisDescription = ModuleDescription{
	Summary: "Asks a Redis server for an answer: the check sends PING and expects PONG. It can log in first.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "connection_security", Type: "string", Required: true, AllowedValues: []any{databaseVerifiedTLS, databaseUnencrypted}, Description: "Required on every check: verified_tls encrypts and verifies the server certificate; unencrypted sends without encryption, including any login."},
		DangerouslySkipServerCertificateCheckInput,
		{Name: "trusted_certificate_authority_file", Type: "string", Description: "With verified_tls: the absolute path of a file of PEM certificates trusted in place of the system authorities."},
		{Name: "address", Type: "string", Required: true, Description: "The address of the Redis server, as host:port."},
		{Name: "login_user", Type: "string", Description: "The account that the check logs in as, on a server that has accounts. It needs login_password_file."},
		{Name: "login_password_file", Type: "string", Description: "The absolute path of a file that holds the password, on one line. MMWatchdog reads it for each run."},
	},
	Examples: []ModuleExample{{
		Description:   "The Redis server of this machine answers.",
		Configuration: "redis:\n  connection_security: unencrypted\n  address: 127.0.0.1:6379\nalert_severity: sos\n",
	}},
}
