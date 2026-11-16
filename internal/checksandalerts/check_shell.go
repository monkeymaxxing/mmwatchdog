package checksandalerts

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"mmwatchdog/internal/primitives"
)

// Shell is the inputs of the shell check module, which runs a Bash script.
// The check is healthy when the script ends with one of SuccessExitCodes.
type Shell struct {
	Script                 string `yaml:"script" json:"script"`
	SuccessExitCodes       []int  `yaml:"success_exit_codes" json:"success_exit_codes"`
	primitives.ExecOptions `yaml:",inline"`
}

// UnmarshalYAML also takes the script alone, written as one string.
func (s *Shell) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		s.Script = n.Value
		return nil
	}
	type plain Shell
	return n.Decode((*plain)(s))
}

func (s *Shell) Validate(InputValidation) *FieldError {
	if strings.TrimSpace(s.Script) == "" {
		return &FieldError{"script", "cannot be empty"}
	}
	if s.SuccessExitCodes == nil {
		s.SuccessExitCodes = []int{0}
	}
	if len(s.SuccessExitCodes) == 0 {
		return &FieldError{"success_exit_codes", "cannot be empty"}
	}
	seen := map[int]bool{}
	for _, code := range s.SuccessExitCodes {
		if code < 0 || code > 255 || seen[code] {
			return &FieldError{"success_exit_codes", "expected distinct integers 0..255"}
		}
		seen[code] = true
	}
	return ValidateExecOptions(&s.ExecOptions)
}

// ValidateOnNode checks that the script can start here: its user account
// and its directory exist.
func (s *Shell) ValidateOnNode() *FieldError { return ValidateExecOptionsOnNode(s.ExecOptions) }

// bashCannotFindOrRun is the line in which Bash says that it cannot find a
// command or a file of that name, which ends the script with 127, or cannot
// run one that it found, such as a script that is not executable, which
// ends it with 126: "bash: line 1: myapp-cli: command not found". A program
// that ends with 127 or 126 by itself writes another line, such as the
// dynamic loader for a missing library: "lame: error while loading shared
// libraries: libmp3lame.so.0: cannot open shared object file: No such file
// or directory".
var bashCannotFindOrRun = regexp.MustCompile(`^[^:]+: (line \d+: )?[^:]+: (command not found|No such file or directory|Permission denied|Is a directory|cannot execute.*|[^:]+: bad interpreter: .*)$`)

func (s *Shell) Run(ctx context.Context, _ *CheckRun) Observation {
	ran, err := primitives.RunCommand(ctx, primitives.Command{Arguments: primitives.Bash(s.Script), RunAs: &s.ExecOptions})
	o := Result("failed", fmt.Sprintf("command exited %d", ran.ExitCode))
	o.Stdout, o.Stderr = ran.StandardOutput, ran.StandardError
	// A run that ended with an error has no exit code that the script wrote.
	if err == nil {
		o.ExitCode = &ran.ExitCode
	}
	errorLines := strings.Split(strings.TrimSpace(ran.StandardError), "\n")
	bashSays := strings.TrimSpace(errorLines[len(errorLines)-1])
	switch {
	case err != nil:
		o.Status, o.Message = "unknown", err.Error()
	case slices.Contains(s.SuccessExitCodes, ran.ExitCode):
		o.Status = "ok"
	// When Bash cannot find or run a command, the script did not look at
	// what it checks, so MMWatchdog cannot check, and a recovery action would not
	// help.
	case ran.ExitCode == 127 && bashCannotFindOrRun.MatchString(bashSays):
		o.Status, o.Message = "unknown", "command exited 127, which Bash uses when it cannot find a command: "+primitives.DetailLine(bashSays)
	case ran.ExitCode == 126 && bashCannotFindOrRun.MatchString(bashSays):
		o.Status, o.Message = "unknown", "command exited 126, which Bash uses when it cannot run a command: "+primitives.DetailLine(bashSays)
	}
	return o
}

// shellDescription is what "mmwatchdog describe-module shell" shows, and what
// validation knows about the module's inputs.
var shellDescription = ModuleDescription{
	Summary: "Runs a Bash script. The check is healthy when the script ends with an exit code that counts as success.",
	UsedAs:  UsedAsCheck,
	Value:   &ModuleInput{Type: "string", Description: "The script alone, in place of the mapping of inputs."},
	Inputs: []ModuleInput{
		{Name: "script", Type: "string", Required: true, Description: "The Bash script. A one-line command and a script of many lines run in the same way."},
		{Name: "success_exit_codes", Type: "list_of_integers", Description: "The exit codes that count as healthy, [0] by default."},
		{Name: "run_as_user_account", Type: "string", Description: "The account that the script runs as, mmwatchdog by default. The mmwatchdog account is unprivileged."},
		{Name: "run_from_directory", Type: "string", Description: "The absolute path of the directory that the script starts in, / by default."},
		{Name: "env", Type: "mapping_of_strings", Description: "Environment variables for the script, beside the standard ones that MMWatchdog gives it."},
	},
	Examples: []ModuleExample{{
		Description:   "The application's own self-test passes.",
		Configuration: "shell: myapp-cli selftest | grep 'all tests passed' > /dev/null\nalert_severity: warn\n",
	}},
}
