package checksandalerts

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// What the postgresql and the mysql check modules share: how a check names
// its database server and its login. The names are those of MMX's modules
// for the same servers.

// DatabaseServer is the inputs of the two modules that say where the server
// is. Without DatabaseServerHost the check uses the server's local socket
// on this node, as the server's own administrator, and needs no password.
// With it, the check's question and the password of its login travel over
// the network, so ConnectionSecurity is required, as it is for a mail
// server: a reader sees on the check how the connection is protected.
type DatabaseServer struct {
	DangerouslySkipServerCertificateCheck  bool   `yaml:"dangerously_skip_server_certificate_check" json:"dangerously_skip_server_certificate_check,omitempty"`
	DatabaseServerHost                     string `yaml:"database_server_host" json:"database_server_host,omitempty"`
	DatabaseServerPort                     int    `yaml:"database_server_port" json:"database_server_port,omitempty"`
	ConnectionSecurity                     string `yaml:"connection_security" json:"connection_security,omitempty"`
	DatabaseServerCertificateAuthorityFile string `yaml:"database_server_certificate_authority_file" json:"database_server_certificate_authority_file,omitempty"`
	LoginPasswordFile                      string `yaml:"login_password_file" json:"login_password_file,omitempty"`
}

// The values of connection_security for a database server. verified_tls
// encrypts the connection and verifies the server's certificate for the
// name in database_server_host unless explicitly disabled. unencrypted never encrypts.
const (
	databaseVerifiedTLS = "verified_tls"
	databaseUnencrypted = "unencrypted"
)

// validate checks the shared inputs. login is the name of the module's own
// input for the account, login_role or login_user, and account its value.
func (d *DatabaseServer) validate(v InputValidation, login, account string) *FieldError {
	switch {
	case v.Authored("database_server_host") && !isHostWithoutPort(d.DatabaseServerHost):
		return &FieldError{"database_server_host", "expected the name or the address of the database server, without a port"}
	case d.DatabaseServerHost != "" && !v.Authored("database_server_port"):
		return &FieldError{"database_server_port", "is required with database_server_host: the port that the database server listens on; add database_server_port: 5432 for PostgreSQL or database_server_port: 3306 for MySQL"}
	case v.Authored("database_server_port") && (d.DatabaseServerPort < 1 || d.DatabaseServerPort > 65535):
		return &FieldError{"database_server_port", "expected a port number from 1 to 65535"}
	case !PlainText(account, 256) || strings.ContainsAny(account, " "):
		return &FieldError{login, "expected one word of at most 256 bytes"}
	case d.LoginPasswordFile != "" && !filepath.IsAbs(d.LoginPasswordFile):
		return &FieldError{"login_password_file", "must be absolute"}
	case d.DatabaseServerHost != "" && (account == "" || d.LoginPasswordFile == ""):
		return &FieldError{"database_server_host", "a check of a server on another machine logs in with a password: state " + login + " and login_password_file"}
	case d.DatabaseServerHost == "" && d.ConnectionSecurity != "":
		return &FieldError{"connection_security", "has no use without database_server_host: the check uses the server's local socket on this node, which has no encryption and needs none"}
	case d.DatabaseServerHost == "" && d.DatabaseServerCertificateAuthorityFile != "":
		return &FieldError{"database_server_certificate_authority_file", "has no use without database_server_host: the check uses the server's local socket on this node, which has no encryption and needs none"}
	case d.DatabaseServerHost != "" && d.ConnectionSecurity != databaseVerifiedTLS && d.ConnectionSecurity != databaseUnencrypted:
		return &FieldError{"connection_security", "required with database_server_host: write connection_security: verified_tls to encrypt the connection, or connection_security: unencrypted to send without encryption"}
	case d.DatabaseServerCertificateAuthorityFile != "" && d.ConnectionSecurity != databaseVerifiedTLS:
		return &FieldError{"database_server_certificate_authority_file", "has a use only with connection_security: verified_tls, which verifies the server's certificate"}
	case d.DatabaseServerCertificateAuthorityFile != "" && !filepath.IsAbs(d.DatabaseServerCertificateAuthorityFile):
		return &FieldError{"database_server_certificate_authority_file", "must be absolute"}
	case d.LoginPasswordFile != "" && account == "":
		return &FieldError{login, "required with login_password_file: the account that the check logs in as"}
	}
	return nil
}

// isHostWithoutPort reports the name or the address of a machine, without a
// port: an address of either family, or a name, which holds no colon.
func isHostWithoutPort(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return host != "" && !strings.ContainsAny(host, " \t\r\n/:[]")
}

// CredentialFiles is the file that holds the password of the login.
func (d *DatabaseServer) CredentialFiles() map[string]string {
	if d.LoginPasswordFile == "" {
		return nil
	}
	return map[string]string{"login_password_file": d.LoginPasswordFile}
}

// ValidateOnNode checks that the password file and the certificate
// authority file can be read here. The check reads the password again for
// each run, and the client program reads the certificates.
func (d *DatabaseServer) ValidateOnNode() *FieldError {
	if d.LoginPasswordFile != "" {
		if _, err := ReadPassword(d.LoginPasswordFile); err != nil {
			return &FieldError{"login_password_file", err.Error()}
		}
	}
	if d.DatabaseServerCertificateAuthorityFile != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if problem := d.certificateAuthorityFileProblem(ctx); problem != "" {
			return &FieldError{"database_server_certificate_authority_file", problem}
		}
	} else if d.ConnectionSecurity == databaseVerifiedTLS && !d.DangerouslySkipServerCertificateCheck {
		if _, problem := systemTrustedCertificateAuthorityFile(); problem != "" {
			return &FieldError{"connection_security", problem}
		}
	}
	return nil
}

// certificateAuthorityFileProblem returns what keeps the client program
// from using the check's certificate authority file, or "". MMWatchdog reads every
// other file of a check itself, as root. This one the client program reads,
// as the unprivileged account, and what the client programs say when they
// cannot read it does not help: psql gives no reason, and MySQL's client
// names a function of its library. So MMWatchdog asks the operating system whether
// that account can read the file.
func (d *DatabaseServer) certificateAuthorityFileProblem(ctx context.Context) string {
	if _, err := readTrustedCertificateAuthorityFile(d.DatabaseServerCertificateAuthorityFile); err != nil {
		return err.Error()
	}
	ran, err := primitives.RunCommand(ctx, primitives.Command{Arguments: []string{"test", "-r", d.DatabaseServerCertificateAuthorityFile}, RunAs: &primitives.ExecOptions{RunAsUserAccount: primitives.UnprivilegedAccount, RunFromDirectory: "/"}})
	if err == nil && ran.ExitCode != 0 {
		return "the account " + primitives.UnprivilegedAccount + ", which runs the client program of the database server, cannot read " + d.DatabaseServerCertificateAuthorityFile + ". A certificate is no secret: give every account the permission to read the file and to pass through the directories above it"
	}
	return ""
}

// systemTrustedCertificateAuthorityFiles are the places where distributions
// keep trusted certificate authorities as one file of PEM certificates:
// Debian, Ubuntu, and Arch Linux, then openSUSE, which has no file at the
// first place, then the Red Hat family, which has neither.
var systemTrustedCertificateAuthorityFiles = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/ca-bundle.pem", "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"}

// systemTrustedCertificateAuthorityFile returns the node's own file of certificate
// authorities, or what is wrong: a node without one cannot verify a
// certificate for a check that states no file of its own.
func systemTrustedCertificateAuthorityFile() (file, problem string) {
	for _, file := range systemTrustedCertificateAuthorityFiles {
		if fileExists(primitives.NodeFile(file)) {
			return primitives.NodeFile(file), ""
		}
	}
	return "", "verified_tls needs the certificate authorities that this node trusts, and this node has no file of them: MMWatchdog looked for " + strings.Join(systemTrustedCertificateAuthorityFiles, ", ") + ". Install the distribution's package of certificate authorities, or state database_server_certificate_authority_file"
}

// trustedCertificateAuthorities returns the file of PEM certificates with
// which the client program of a check with verified_tls verifies the
// server's certificate: the check's own file, or the system's. The check
// always names a file, because a client program does not by itself look in
// the system's file: MySQL's verifies nothing without one, and psql knows
// the system's only from PostgreSQL 16. A problem is the observation of the
// check.
func (d *DatabaseServer) trustedCertificateAuthorities(ctx context.Context) (string, *Observation) {
	if d.DatabaseServerCertificateAuthorityFile != "" {
		if problem := d.certificateAuthorityFileProblem(ctx); problem != "" {
			o := Result("unknown", "database_server_certificate_authority_file: "+problem)
			return "", &o
		}
		return d.DatabaseServerCertificateAuthorityFile, nil
	}
	if d.DangerouslySkipServerCertificateCheck {
		return "", nil
	}
	file, problem := systemTrustedCertificateAuthorityFile()
	if problem != "" {
		o := Result("unknown", "connection_security: "+problem)
		return "", &o
	}
	return file, nil
}

// databaseServerInputs are the descriptions of the shared inputs. server is
// the name of the database server for the texts, and administrator says how
// the check logs in through the local socket.
func databaseServerInputs(server, login, administrator string) []ModuleInput {
	return []ModuleInput{
		{Name: "database_server_host", Type: "string", Description: "The name or the address of the " + server + " server. Without it the check uses the server's local socket on this node, " + administrator + "."},
		DangerouslySkipServerCertificateCheckInput,
		{Name: "database_server_port", Type: "integer", Description: "Required with database_server_host: the port that the server listens on, such as 5432 for PostgreSQL or 3306 for MySQL."},
		{Name: "connection_security", Type: "string", AllowedValues: []any{databaseVerifiedTLS, databaseUnencrypted},
			Description: "How the connection to database_server_host is protected. Required with database_server_host, and of no use without it. verified_tls encrypts the connection and verifies the server's certificate for database_server_host unless dangerously_skip_server_certificate_check is true; unencrypted never encrypts."},
		{Name: "database_server_certificate_authority_file", Type: "string", Description: "With verified_tls: the absolute path of a file of PEM certificates that the check trusts in place of the system's. The client program reads the file as the account mmwatchdog, so every account must be able to read it."},
		{Name: login, Type: "string", Description: "The account inside the " + server + " server that the check logs in as. Required with database_server_host."},
		{Name: "login_password_file", Type: "string", Description: "The absolute path of a file that holds the password of that account, on one line. MMWatchdog reads it for each run. Required with database_server_host."},
	}
}

// askDatabaseClient runs the client program of a database server with one
// question and returns what the client wrote. account is the operating
// system account that runs the client, and environment holds the password
// when the check has one: a password never is an argument, because the
// arguments of a program show in the process list. A problem is the
// observation of the check: unknown when the client cannot run, and failed
// when the server refuses or does not answer.
func askDatabaseClient(ctx context.Context, server, account string, arguments []string, environment map[string]string) (string, *Observation) {
	problem := func(status, message string) (string, *Observation) {
		o := Result(status, primitives.DetailLine(message))
		return "", &o
	}
	ran, err := primitives.RunCommand(ctx, primitives.Command{Arguments: arguments, RunAs: &primitives.ExecOptions{RunAsUserAccount: account, RunFromDirectory: "/", Env: environment}})
	var notFound *exec.Error
	switch {
	case errors.As(err, &notFound):
		return problem("unknown", fmt.Sprintf("MMWatchdog cannot run %s, the client program of %s: %v. The check needs that program on this node", arguments[0], server, notFound.Err))
	case err != nil && ctx.Err() != nil:
		return problem("failed", "the "+server+" server did not answer within the time limit of the run")
	case err != nil:
		return problem("unknown", "MMWatchdog cannot run "+arguments[0]+" as the account "+account+": "+err.Error())
	case ran.ExitCode != 0:
		said := strings.TrimSpace(ran.StandardError)
		if said == "" {
			said = arguments[0] + " ended with exit code " + strconv.Itoa(ran.ExitCode)
		}
		o := Result("failed", "the "+server+" server did not answer the check: "+clientLine(whatTheClientSays(said)))
		return "", &o
	}
	return ran.StandardOutput, nil
}

// clientLineStart and clientLineEnd are the most bytes that a message keeps
// of the start and of the end of a client program's line.
const clientLineStart, clientLineEnd = 100, 200

// clientLine makes the line of a client program fit a message. A client
// program puts the reason at the end of its line, behind the name and the
// address of the server, so a line that is too long loses its middle and
// keeps its end: cut at its end, the line of psql for a server with a long
// name would say where the server is and not what is wrong.
func clientLine(line string) string {
	line = primitives.OnOneLine(line)
	if len(line) <= clientLineStart+clientLineEnd {
		return line
	}
	return strings.TrimSpace(strings.ToValidUTF8(line[:clientLineStart], "")) + " ... " + strings.TrimSpace(strings.ToValidUTF8(line[len(line)-clientLineEnd:], ""))
}

// whatTheClientSays picks, from what a client program wrote about a failed
// question, the line that says what is wrong: the first line that is no
// warning. The client's hints follow that line, and a client can write a
// warning in front of it.
func whatTheClientSays(written string) string {
	lines := strings.Split(strings.TrimSpace(written), "\n")
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(strings.ToUpper(line), "WARNING") {
			return line
		}
	}
	return strings.TrimSpace(lines[0])
}

// password reads the password of the check's login, or returns "" for a
// check without one. A problem is the observation of the check.
func (d *DatabaseServer) password() (string, *Observation) {
	if d.LoginPasswordFile == "" {
		return "", nil
	}
	password, err := ReadPassword(d.LoginPasswordFile)
	if err != nil {
		o := Result("unknown", err.Error())
		return "", &o
	}
	return password, nil
}
