package checksandalerts

// DangerouslySkipServerCertificateCheckInput is the shared description for
// modules that explicitly offer the option to skip server authentication.
// Modules opt in by adding this input to their descriptions and input types.
var DangerouslySkipServerCertificateCheckInput = ModuleInput{
	Name: "dangerously_skip_server_certificate_check", Type: "boolean",
	Description: "With true, encrypt the connection without checking the server's certificate or identity. By default, verify the server's certificate.",
}
