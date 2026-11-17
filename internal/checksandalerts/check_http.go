package checksandalerts

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"mmwatchdog/internal/primitives"
)

// HTTP is the inputs of the http check module, which sends one request and
// checks the answer. Credentials come only from BearerTokenFile or
// BasicAuthFile, read for each request.
type HTTP struct {
	DangerouslySkipServerCertificateCheck bool              `yaml:"dangerously_skip_server_certificate_check" json:"dangerously_skip_server_certificate_check,omitempty"`
	URL                                   string            `yaml:"url" json:"url"`
	Method                                string            `yaml:"method" json:"method"`
	Headers                               map[string]string `yaml:"headers" json:"headers,omitempty"`
	BearerTokenFile                       string            `yaml:"bearer_token_file" json:"bearer_token_file,omitempty"`
	BasicAuthFile                         string            `yaml:"basic_auth_file" json:"basic_auth_file,omitempty"`
	TrustedCertificateAuthorityFile       string            `yaml:"trusted_certificate_authority_file" json:"trusted_certificate_authority_file,omitempty"`
	ExpectedHTTPStatusCodes               []int             `yaml:"expected_http_status_codes" json:"expected_http_status_codes"`
	ResponseBodyContains                  string            `yaml:"response_body_contains" json:"response_body_contains,omitempty"`
	ResponseBodyDoesNotContain            string            `yaml:"response_body_does_not_contain" json:"response_body_does_not_contain,omitempty"`
	ResponseBodyMatchesRegex              string            `yaml:"response_body_matches_regex" json:"response_body_matches_regex,omitempty"`
}

var headerName = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")

func (h *HTTP) Validate(v InputValidation) *FieldError {
	for _, field := range []struct{ name, value string }{{"response_body_contains", h.ResponseBodyContains}, {"response_body_does_not_contain", h.ResponseBodyDoesNotContain}, {"response_body_matches_regex", h.ResponseBodyMatchesRegex}} {
		if v.Authored(field.name) && field.value == "" {
			return &FieldError{field.name, "cannot be empty: set a nonempty value or remove " + field.name}
		}
	}
	if !httpURL(h.URL) {
		return &FieldError{"url", "expected HTTP(S) URL without credentials and with a host, such as https://example.com/health"}
	}
	if h.ExpectedHTTPStatusCodes == nil {
		h.ExpectedHTTPStatusCodes = []int{200}
	}
	if len(h.ExpectedHTTPStatusCodes) == 0 {
		return &FieldError{"expected_http_status_codes", "cannot be empty: list the HTTP status codes that count as healthy, such as [200]"}
	}
	codes := map[int]bool{}
	for _, code := range h.ExpectedHTTPStatusCodes {
		if code < 100 || code > 599 || codes[code] {
			return &FieldError{"expected_http_status_codes", "expected distinct HTTP status codes from 100 to 599, such as [200]"}
		}
		codes[code] = true
	}
	if _, e := regexp.Compile(h.ResponseBodyMatchesRegex); e != nil {
		return &FieldError{"response_body_matches_regex", e.Error()}
	}
	if h.Method == "" {
		h.Method = "GET"
	}
	if h.Method != "GET" && h.Method != "HEAD" {
		return &FieldError{"method", "must be GET or HEAD"}
	}
	if h.Method == "HEAD" && h.hasContentRules() {
		return &FieldError{"method", "HEAD cannot check response body content; remove the response body rules or use GET"}
	}
	seen := map[string]bool{}
	for _, name := range Names(h.Headers) {
		key := http.CanonicalHeaderKey(name)
		if !headerName.MatchString(name) || seen[key] {
			return &FieldError{"headers." + name, "expected a distinct HTTP header name"}
		}
		if key == "Authorization" {
			return &FieldError{"headers." + name, "use bearer_token_file or basic_auth_file, so the secret stays out of configuration"}
		}
		if !PlainText(h.Headers[name], 4096) {
			return &FieldError{"headers." + name, "expected text without control characters, up to 4096 bytes"}
		}
		seen[key] = true
	}
	if h.BearerTokenFile != "" && h.BasicAuthFile != "" {
		return &FieldError{"basic_auth_file", "set only one of bearer_token_file and basic_auth_file"}
	}
	files := map[string]string{"bearer_token_file": h.BearerTokenFile, "basic_auth_file": h.BasicAuthFile, "trusted_certificate_authority_file": h.TrustedCertificateAuthorityFile}
	for _, field := range Names(files) {
		if files[field] != "" && !filepath.IsAbs(files[field]) {
			return &FieldError{field, "must be absolute"}
		}
	}
	return nil
}

// hasContentRules reports a check that looks into the response body.
func (h *HTTP) hasContentRules() bool {
	return h.ResponseBodyContains != "" || h.ResponseBodyDoesNotContain != "" || h.ResponseBodyMatchesRegex != ""
}

// CredentialFiles are the files that hold the token or the user name and
// password of the request.
func (h *HTTP) CredentialFiles() map[string]string {
	files := map[string]string{}
	if h.BearerTokenFile != "" {
		files["bearer_token_file"] = h.BearerTokenFile
	}
	if h.BasicAuthFile != "" {
		files["basic_auth_file"] = h.BasicAuthFile
	}
	return files
}

// ValidateOnNode checks that the credential file and the certificate
// authority file can be read here. The check reads them again for each
// request.
func (h *HTTP) ValidateOnNode() *FieldError {
	if h.BearerTokenFile != "" {
		if _, err := ReadBearerToken(h.BearerTokenFile); err != nil {
			return &FieldError{"bearer_token_file", err.Error()}
		}
	}
	if h.BasicAuthFile != "" {
		if _, _, err := readBasicAuth(h.BasicAuthFile); err != nil {
			return &FieldError{"basic_auth_file", err.Error()}
		}
	}
	if h.TrustedCertificateAuthorityFile != "" {
		if _, err := readTrustedCertificateAuthorityFile(h.TrustedCertificateAuthorityFile); err != nil {
			return &FieldError{"trusted_certificate_authority_file", err.Error()}
		}
	}
	return nil
}

// Run sends one request. A credential file or certificate authority file
// that cannot be read leaves health unknown; every answer the endpoint
// gives, or fails to give, is failed or ok.
func (h *HTTP) Run(ctx context.Context, r *CheckRun) Observation {
	method := h.Method
	if method == "" {
		method = "GET"
	}
	// The check reads the body only of an answer whose status code it
	// expects, and only for a rule about the content: an answer with another
	// status code fails the check at once, also when its body is slow.
	request := primitives.HTTPRequestToSend{DangerouslySkipServerCertificateCheck: h.DangerouslySkipServerCertificateCheck, Method: method, URL: h.URL, Headers: maps.Clone(h.Headers), ReadsResponseBody: func(statusCode int) bool {
		return h.hasContentRules() && slices.Contains(h.ExpectedHTTPStatusCodes, statusCode)
	}}
	if request.Headers == nil {
		request.Headers = map[string]string{}
	}
	switch {
	case h.BearerTokenFile != "":
		token, err := ReadBearerToken(h.BearerTokenFile)
		if err != nil {
			return Result("unknown", err.Error())
		}
		request.Headers["Authorization"] = "Bearer " + token
	case h.BasicAuthFile != "":
		user, password, err := readBasicAuth(h.BasicAuthFile)
		if err != nil {
			return Result("unknown", err.Error())
		}
		request.Headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
	}
	if h.TrustedCertificateAuthorityFile != "" {
		roots, err := readTrustedCertificateAuthorityFile(h.TrustedCertificateAuthorityFile)
		if err != nil {
			return Result("unknown", err.Error())
		}
		request.CertificateAuthorities = roots
	}
	response, err := primitives.HTTPRequest(ctx, request)
	// A request that the time limit of the run ended has failed. Go would
	// say "context deadline exceeded".
	timedOut := err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)
	if timedOut && response.StatusCode == 0 {
		return Result("failed", fmt.Sprintf("%s%s %q: no answer within %s", method[:1], strings.ToLower(method[1:]), addressWithoutQuery(h.URL), timeLimitWords(r)))
	}
	if err != nil && response.StatusCode == 0 {
		// The message names the address in the form that Go gives an error
		// of a request, without its query, which can hold a token.
		return Result("failed", fmt.Sprintf("%s%s %q: %s", method[:1], strings.ToLower(method[1:]), addressWithoutQuery(h.URL), err))
	}
	if !slices.Contains(h.ExpectedHTTPStatusCodes, response.StatusCode) {
		return Result("failed", fmt.Sprintf("expected HTTP %s, received %d", httpStatusCodeList(h.ExpectedHTTPStatusCodes), response.StatusCode))
	}
	switch {
	case errors.Is(err, primitives.ErrHTTPResponseBodyLimit):
		return Result("failed", "HTTP response body exceeds the 1 MiB limit for checking content")
	case timedOut:
		return Result("failed", "the HTTP response body did not arrive within "+timeLimitWords(r))
	case err != nil:
		return Result("failed", err.Error())
	case h.ResponseBodyContains != "" && !bytes.Contains(response.Body, []byte(h.ResponseBodyContains)):
		return Result("failed", "HTTP response body does not contain the text in response_body_contains")
	case h.ResponseBodyDoesNotContain != "" && bytes.Contains(response.Body, []byte(h.ResponseBodyDoesNotContain)):
		return Result("failed", "HTTP response body contains the text in response_body_does_not_contain")
	case h.ResponseBodyMatchesRegex != "" && !regexp.MustCompile(h.ResponseBodyMatchesRegex).Match(response.Body):
		return Result("failed", "HTTP response body does not match response_body_matches_regex")
	}
	return Result("ok", fmt.Sprintf("HTTP %d and configured content match", response.StatusCode))
}

// httpStatusCodeList writes the accepted status codes for a message: "200",
// "200 or 302", or "200, 301, or 302".
func httpStatusCodeList(codes []int) string {
	words := make([]string, len(codes))
	for i, code := range codes {
		words[i] = strconv.Itoa(code)
	}
	switch len(words) {
	case 1:
		return words[0]
	case 2:
		return words[0] + " or " + words[1]
	}
	return strings.Join(words[:len(words)-1], ", ") + ", or " + words[len(words)-1]
}

// HTTPDescription is what "mmwatchdog describe-module http" shows, and what
// validation knows about the module's inputs.
var HTTPDescription = ModuleDescription{
	Summary: "Sends one HTTP or HTTPS request and checks the status code and, when asked, the content of the answer.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "url", Type: "string", Required: true, Description: "The address to request, with http:// or https://, without a user name or a password in it."},
		DangerouslySkipServerCertificateCheckInput,
		{Name: "method", Type: "string", AllowedValues: []any{"GET", "HEAD"}, Description: "The request method, GET by default. HEAD cannot check response body content."},
		{Name: "headers", Type: "mapping_of_strings", Description: "Request headers to send. A Host header selects the virtual host and, for HTTPS, the name that is verified. An Authorization header is refused: use bearer_token_file or basic_auth_file."},
		{Name: "bearer_token_file", Type: "string", Description: "The absolute path of a file that holds a bearer token. MMWatchdog reads it for each request."},
		{Name: "basic_auth_file", Type: "string", Description: "The absolute path of a file that holds one user:password line. MMWatchdog reads it for each request."},
		{Name: "trusted_certificate_authority_file", Type: "string", Description: "The absolute path of a file of PEM certificates that MMWatchdog trusts for this check in place of the system's."},
		{Name: "expected_http_status_codes", Type: "list_of_integers", Description: "The HTTP status codes that count as healthy, [200] by default."},
		{Name: "response_body_contains", Type: "string", Description: "A text that the answer must hold."},
		{Name: "response_body_does_not_contain", Type: "string", Description: "A text that the answer must not hold."},
		{Name: "response_body_matches_regex", Type: "string", Description: "A regular expression that the answer must match."},
	},
	Examples: []ModuleExample{{
		Description:   "An application answers its health request with 200 and the word ready.",
		Configuration: "http:\n  url: http://127.0.0.1:8080/health\n  expected_http_status_codes: [200]\n  response_body_contains: ready\nalert_severity: sos\n",
	}},
}

// addressWithoutQuery is the address of an http check for a message: the
// query and the fragment become "?...", because a query can hold a token
// and the message goes to every alert destination.
func addressWithoutQuery(address string) string {
	if i := strings.IndexAny(address, "?#"); i >= 0 {
		return address[:i] + "?..."
	}
	return address
}
