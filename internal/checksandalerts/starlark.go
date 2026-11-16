package checksandalerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	starlarkjson "go.starlark.net/lib/json"
	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"

	"mmwatchdog/internal/primitives"
)

// An official extra module, and a module of the project's own that is no
// executable, is a Starlark script: the file module.star in the module's
// directory. MMWatchdog runs a script in a process of its own: the daemon starts
// "mmwatchdog run-starlark-module DIRECTORY" as the account of the check or the
// destination, and that process runs the script here. The process speaks
// the JSON protocol of an executable module, so the daemon runs both in one
// way. A script touches the node only through the primitives below, which
// are the functions of primitives.go under their names for scripts. The
// rules are in docs/changing-mmwatchdog/design/checks.md under "Modules".

const (
	// ModuleScriptFile is the name of a module's script, and
	// moduleScriptLimit the most bytes that it holds.
	ModuleScriptFile  = "module.star"
	moduleScriptLimit = 1024 * 1024
	// starlarkStepLimit is the most steps that one run of a script takes. A
	// check needs far fewer; the limit ends a script that counts without
	// end before it keeps a processor busy until the time limit.
	starlarkStepLimit = 10_000_000
	// ModuleStateFile holds, in a check's state directory, the value that
	// its script stored with write_state, and ModuleStateLimit is the most
	// bytes that the value is as JSON.
	ModuleStateFile  = "state.json"
	ModuleStateLimit = 1024 * 1024
	// ScriptReadFileLimit is the most bytes that read_file reads.
	ScriptReadFileLimit = 1024 * 1024
	// scriptPrintLimit is the most bytes that MMWatchdog passes on of what a script
	// prints. MMWatchdog keeps DiagnosticLimit bytes of a module's standard error,
	// and the error of a script is the last line there, so what the script
	// prints gets half of that room and the error always arrives.
	scriptPrintLimit = primitives.DiagnosticLimit / 2
	// timeForTheResultAfterTheCalls is the most time that a script keeps
	// for its result after the end of its calls: the calls of the
	// primitives end a tenth of the module's time before MMWatchdog ends the
	// module, and at most this long before it. In that time this process
	// ends the call, the script decides its result, and the result reaches
	// MMWatchdog, also on a busy node.
	timeForTheResultAfterTheCalls = time.Second
)

// RunStarlarkModule runs the script of the module in directory once. It
// reads one request of the JSON protocol from in and writes one result to
// out. A request with an alert calls the script's deliver_alert, and every
// other request its run_check. An error says what is wrong with the request
// or the script; the caller reports it and ends with an exit status other
// than zero.
func RunStarlarkModule(ctx context.Context, directory string, in io.Reader, out io.Writer) error {
	received, e := io.ReadAll(io.LimitReader(in, ModuleRequestLimit+1))
	if e != nil {
		return e
	}
	if len(received) > ModuleRequestLimit {
		return fmt.Errorf("the request is more than %d bytes", ModuleRequestLimit)
	}
	var request map[string]any
	decoder := json.NewDecoder(bytes.NewReader(received))
	decoder.UseNumber()
	if e := decoder.Decode(&request); e != nil {
		return fmt.Errorf("the request is not one JSON object: %w", e)
	}
	if protocol, _ := request["protocol"].(json.Number); protocol.String() != "1" {
		return errors.New("the module request must use protocol: 1")
	}
	path := filepath.Join(directory, ModuleScriptFile)
	source, e := primitives.ReadFile(path, moduleScriptLimit)
	if e != nil {
		return e
	}
	stateDirectory, _ := request["state_dir"].(string)
	_, deliversAlerts := request["alert"]
	run := &scriptRun{ctx: ctx, stateDirectory: stateDirectory}
	// MMWatchdog ends the module at the time limit of the run. Each call of a
	// primitive ends shortly before then, with an error, so that the script
	// still decides its result: a server that accepts the connection and
	// gives no answer then fails a check that treats the error as a failure.
	if stated, given := request["mmwatchdog_ends_the_module_after_milliseconds"]; given {
		written, isNumber := stated.(json.Number)
		milliseconds, e := written.Int64()
		if !isNumber || e != nil {
			return errors.New("mmwatchdog_ends_the_module_after_milliseconds in the request must be a whole number")
		}
		left := time.Duration(milliseconds) * time.Millisecond
		var cancel context.CancelFunc
		run.ctx, cancel = context.WithTimeout(ctx, left-min(left/10, timeForTheResultAfterTheCalls))
		defer cancel()
		run.timeLimit = "the time limit of the run (time_limit_per_run)"
		if deliversAlerts {
			run.timeLimit = "the time limit of the delivery attempt (time_limit_per_delivery_attempt)"
		}
	}
	// What a script prints goes to this process's standard error, like the
	// diagnostics of an executable, up to scriptPrintLimit bytes.
	printed := 0
	thread := &starlark.Thread{Name: ModuleScriptFile, Print: func(_ *starlark.Thread, text string) {
		switch before := printed; {
		case before > scriptPrintLimit:
		case before+len(text)+1 > scriptPrintLimit:
			printed = scriptPrintLimit + 1
			fmt.Fprintf(os.Stderr, "[MMWatchdog leaves out what the script printed after its first %d bytes]\n", scriptPrintLimit)
		default:
			printed += len(text) + 1
			fmt.Fprintln(os.Stderr, text)
		}
	}}
	thread.SetMaxExecutionSteps(starlarkStepLimit)
	tooManySteps := false
	thread.OnMaxSteps = func(t *starlark.Thread) {
		tooManySteps = true
		t.Cancel("too many steps")
	}
	// Starlark's default file options stay in force: no while loop, no
	// recursion, and no control flow at the top level. Thread.Load is nil,
	// so a script cannot load another file.
	globals, e := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, ModuleScriptFile, source, run.predeclared())
	if verifying, _ := request["verify_destination"].(bool); verifying && e == nil {
		result := DestinationVerification{VerificationMessage: TestedOnlyByAMessage}
		if hook, exists := globals["verify_destination"]; exists {
			function, ok := hook.(*starlark.Function)
			if !ok {
				return fmt.Errorf("%s: verify_destination must be a function", path)
			}
			inputs, _ := request["inputs"].(map[string]any)
			if inputs == nil {
				inputs = map[string]any{}
			}
			contents, _ := request["file_contents"].(map[string]any)
			if contents == nil {
				contents = map[string]any{}
			}
			answer, err := starlark.Call(thread, function, starlark.Tuple{toStarlark(inputs), toStarlark(contents)}, nil)
			if err != nil {
				return scriptError(path, err)
			}
			tuple, ok := answer.(starlark.Tuple)
			if !ok || len(tuple) != 2 {
				return fmt.Errorf("%s: verify_destination must return (accepted, message)", path)
			}
			accepted, isBool := tuple[0].(starlark.Bool)
			message, isText := starlark.AsString(tuple[1])
			if !isBool || !isText {
				return fmt.Errorf("%s: verify_destination must return a boolean and a one-line text", path)
			}
			result = DestinationVerification{true, bool(accepted), message}
		}
		return json.NewEncoder(out).Encode(struct {
			Protocol int `json:"protocol"`
			DestinationVerification
		}{1, result})
	}
	function, arguments := scriptFunction(deliversAlerts)
	var answer starlark.Value
	if e == nil {
		entry, defined := globals[function].(*starlark.Function)
		if !defined {
			return fmt.Errorf("%s defines no function %s(%s)", path, function, strings.Join(arguments, ", "))
		}
		values := starlark.Tuple{}
		for _, name := range arguments {
			value, _ := request[name].(map[string]any)
			values = append(values, toStarlark(value))
		}
		answer, e = starlark.Call(thread, entry, values, nil)
	}
	if tooManySteps {
		return fmt.Errorf("%s exceeded the limit of %d steps per script run", path, starlarkStepLimit)
	}
	if e != nil {
		return scriptError(path, e)
	}
	result, isDictionary := answer.(*starlark.Dict)
	if !isDictionary {
		return fmt.Errorf("%s: %s returned %s, and it must return a dictionary with a status and a message", path, function, answer.Type()) //nolint:nilaway // Starlark Call rejects a nil result without an error; the error guard returns before answer.Type.
	}
	written, e := fromStarlark(result)
	if e != nil {
		return fmt.Errorf("%s: %s returned a result that MMWatchdog cannot write as JSON: %w", path, function, e)
	}
	fields := written.(map[string]any)
	fields["protocol"] = 1
	encoded, e := PlainJSON(fields)
	if e != nil {
		return e
	}
	_, e = out.Write(append(encoded, '\n'))
	return e
}

// scriptFunction names the function of a script that MMWatchdog calls, with the
// names of its arguments: run_check for a check, and deliver_alert for an
// alert destination.
func scriptFunction(deliversAlerts bool) (string, []string) {
	if deliversAlerts {
		return "deliver_alert", []string{"alert", "inputs", "file_contents"}
	}
	return "run_check", []string{"inputs", "file_contents"}
}

// checkScript reads the script of the module in directory and says what is
// wrong with it, without running it: text that is no Starlark, a name that
// the script uses and nothing defines, and a script that does not define
// the function that MMWatchdog calls. "mmwatchdog validate" checks each script
// that the configuration names this way, so a script with such an error
// stops a deploy, where it would otherwise make every run of its check
// unknown.
func checkScript(directory string, deliversAlerts bool) error {
	path := filepath.Join(directory, ModuleScriptFile)
	source, e := primitives.ReadFile(path, moduleScriptLimit)
	if e != nil {
		return e
	}
	file, e := (&syntax.FileOptions{}).Parse(ModuleScriptFile, source, 0)
	if e != nil {
		return scriptError(path, e)
	}
	if e := resolve.File(file, (&scriptRun{}).predeclared().Has, starlark.Universe.Has); e != nil {
		return scriptError(path, e)
	}
	// The function that MMWatchdog calls is what the name stands for at the end of
	// the script: a def, or a name that the script gives to a def or to a
	// lambda. parameters stay nil for a name that stands for anything else.
	function, arguments := scriptFunction(deliversAlerts)
	type definedFunction struct {
		line       int32
		parameters []syntax.Expr
	}
	functions := map[string]*definedFunction{}
	for _, statement := range file.Stmts {
		switch defined := statement.(type) {
		case *syntax.LoadStmt:
			return fmt.Errorf("%s:%d: a script is one file and cannot load another file of code", path, defined.Load.Line)
		case *syntax.DefStmt:
			functions[defined.Name.Name] = &definedFunction{defined.Def.Line, defined.Params}
		case *syntax.AssignStmt:
			name, isName := defined.LHS.(*syntax.Ident)
			if !isName {
				continue
			}
			delete(functions, name.Name)
			switch value := defined.RHS.(type) {
			case *syntax.LambdaExpr:
				functions[name.Name] = &definedFunction{value.Lambda.Line, value.Params}
			case *syntax.Ident:
				if other := functions[value.Name]; other != nil {
					functions[name.Name] = other
				}
			}
		}
	}
	called := functions[function]
	if called == nil {
		return fmt.Errorf("%s defines no function %s(%s)", path, function, strings.Join(arguments, ", "))
	}
	if !takesArguments(called.parameters, len(arguments)) {
		return fmt.Errorf("%s:%d: MMWatchdog calls %s(%s), and the function of the script does not take these arguments", path, called.line, function, strings.Join(arguments, ", "))
	}
	return nil
}

// takesArguments reports whether a function with these parameters can be
// called with count arguments that are given by their position.
func takesArguments(parameters []syntax.Expr, count int) bool {
	required, withDefault, anyNumber, afterStar := 0, 0, false, false
	for _, parameter := range parameters {
		switch stated := parameter.(type) {
		case *syntax.Ident:
			// A parameter without a default after a star must be given by
			// its name, which MMWatchdog does not do.
			if afterStar {
				return false
			}
			required++
		case *syntax.BinaryExpr:
			if !afterStar {
				withDefault++
			}
		case *syntax.UnaryExpr:
			if stated.Op == syntax.STAR {
				afterStar, anyNumber = true, stated.X != nil
			}
		}
	}
	return count >= required && (anyNumber || count <= required+withDefault)
}

// scriptError writes an error of a script on one line, with the place in
// the script where it happened.
func scriptError(path string, e error) error {
	var wrongSyntax syntax.Error
	if errors.As(e, &wrongSyntax) {
		return fmt.Errorf("%s:%d: %s", path, wrongSyntax.Pos.Line, wrongSyntax.Msg)
	}
	var unresolved resolve.ErrorList
	if errors.As(e, &unresolved) && len(unresolved) > 0 {
		return fmt.Errorf("%s:%d: %s", path, unresolved[0].Pos.Line, unresolved[0].Msg)
	}
	var failed *starlark.EvalError
	if errors.As(e, &failed) {
		// A script can fail with a text of several lines. The error stays
		// one line, because MMWatchdog takes the last line that this process writes
		// as the error of the script.
		failed.Msg = OneLine(failed.Msg)
		// The innermost frame of the script is where the error happened; a
		// frame of a primitive has no place in a file. At(0) is the
		// innermost frame.
		for i := 0; i < len(failed.CallStack); i++ {
			if frame := failed.CallStack.At(i); frame.Pos.Filename() == ModuleScriptFile {
				return fmt.Errorf("%s:%d: %s", path, frame.Pos.Line, failed.Msg)
			}
		}
		return fmt.Errorf("%s: %s", path, failed.Msg)
	}
	return fmt.Errorf("%s: %w", path, e)
}

// scriptRun is what the primitives of one run of a script share.
type scriptRun struct {
	// ctx ends shortly before MMWatchdog ends the module, when MMWatchdog states that time.
	ctx context.Context
	// timeLimit names the time limit at which MMWatchdog ends the module, for the
	// error of a call that ctx ended.
	timeLimit string
	// stateDirectory is the state directory of the check, or "" for a
	// destination, which keeps no state.
	stateDirectory string
}

// endedShortlyBeforeTheTimeLimit is the error of a call of a primitive. A
// call that failed because ctx ended says so, in place of Go's words, and
// what names the call, such as "the call" or "the program". Any other error
// stays as it is.
func (r *scriptRun) endedShortlyBeforeTheTimeLimit(e error, what string) error {
	if e == nil || !errors.Is(r.ctx.Err(), context.DeadlineExceeded) {
		return e
	}
	return fmt.Errorf("MMWatchdog ended %s shortly before %s", what, r.timeLimit)
}

// predeclared are the names that a script can use beside Starlark's own:
// the primitives, and json with its encode and decode.
func (r *scriptRun) predeclared() starlark.StringDict {
	return starlark.StringDict{
		"read_file":         starlark.NewBuiltin("read_file", r.readFile),
		"run_command":       starlark.NewBuiltin("run_command", r.runCommand),
		"http_request":      starlark.NewBuiltin("http_request", r.httpRequest),
		"verify_connection": starlark.NewBuiltin("verify_connection", r.verifyConnection),
		"tcp_exchange":      starlark.NewBuiltin("tcp_exchange", r.tcpExchange),
		"udp_exchange":      starlark.NewBuiltin("udp_exchange", r.udpExchange),
		"read_state":        starlark.NewBuiltin("read_state", r.readState),
		"write_state":       starlark.NewBuiltin("write_state", r.writeState),
		"json":              starlarkjson.Module,
	}
}

// primitiveResult is what a primitive returns to a script: a value with
// named fields, of which error is "" when the call worked.
func primitiveResult(e error, fields starlark.StringDict) starlark.Value {
	problem := ""
	if e != nil {
		problem = e.Error()
	}
	fields["error"] = starlark.String(problem)
	return starlarkstruct.FromStringDict(starlarkstruct.Default, fields)
}

func (r *scriptRun) readFile(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path string
	if e := starlark.UnpackArgs(b.Name(), args, kwargs, "path", &path); e != nil {
		return nil, e
	}
	content, e := primitives.ReadFile(path, ScriptReadFileLimit)
	return primitiveResult(e, starlark.StringDict{"content": starlark.String(content)}), nil
}

// scriptProgramOutputLimit is the most that a script reads of the standard
// output of a program that it runs: as much as it reads of a file and of
// the body of an HTTP response. The report of a tool about a machine with
// many disks is longer than the result of a check may be.
const scriptProgramOutputLimit = 1024 * 1024

func (r *scriptRun) runCommand(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var arguments *starlark.List
	standardInput := ""
	if e := starlark.UnpackArgs(b.Name(), args, kwargs, "arguments", &arguments, "standard_input?", &standardInput); e != nil {
		return nil, e
	}
	program := []string{}
	for i := 0; i < arguments.Len(); i++ {
		argument, isString := starlark.AsString(arguments.Index(i))
		if !isString {
			return nil, fmt.Errorf("%s: arguments must be a list of strings, such as [\"systemctl\", \"is-active\", \"myapp.service\"]", b.Name())
		}
		program = append(program, argument)
	}
	if len(program) == 0 {
		return nil, fmt.Errorf("%s: arguments must name the program to run", b.Name())
	}
	// The process that runs the script has the account, the directory, and
	// the environment of the module already, so the program runs as this
	// process, with the limits of a check's program. The program stays in
	// this process's process group, so the time limit of the check or of
	// the delivery ends it together with the script.
	ran, e := primitives.RunCommand(r.ctx, primitives.Command{Arguments: program, StandardInput: []byte(standardInput), AsMMWatchdogItself: true, StandardOutputLimit: scriptProgramOutputLimit, StaysInThisProcessGroup: true})
	if errors.Is(e, primitives.ErrStandardOutputLimit) {
		e = errors.New("the program wrote more than 1 MiB to its standard output")
	}
	e = r.endedShortlyBeforeTheTimeLimit(e, "the program")
	return primitiveResult(e, starlark.StringDict{
		"exit_code":       starlark.MakeInt(ran.ExitCode),
		"standard_output": starlark.String(ran.StandardOutput),
		"standard_error":  starlark.String(ran.StandardError),
	}), nil
}

func (r *scriptRun) httpRequest(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var method, address string
	headers := &starlark.Dict{}
	body, certificateAuthorityFile := "", ""
	if e := starlark.UnpackArgs(b.Name(), args, kwargs, "method", &method, "url", &address, "headers?", &headers, "body?", &body, "trusted_certificate_authority_file?", &certificateAuthorityFile); e != nil {
		return nil, e
	}
	request := primitives.HTTPRequestToSend{Method: method, URL: address, Headers: map[string]string{}, Body: []byte(body)}
	for _, item := range headers.Items() {
		name, nameIsString := starlark.AsString(item[0])
		value, valueIsString := starlark.AsString(item[1])
		if !nameIsString || !valueIsString {
			return nil, fmt.Errorf("%s: headers must be a dictionary of strings", b.Name())
		}
		request.Headers[name] = value
	}
	empty := starlark.StringDict{"http_status_code": starlark.MakeInt(0), "headers": &starlark.Dict{}, "body": starlark.String("")}
	if certificateAuthorityFile != "" {
		roots, e := readTrustedCertificateAuthorityFile(certificateAuthorityFile)
		if e != nil {
			return primitiveResult(e, empty), nil
		}
		request.CertificateAuthorities = roots
	}
	response, e := primitives.HTTPRequest(r.ctx, request)
	e = r.endedShortlyBeforeTheTimeLimit(e, "the call")
	answered := &starlark.Dict{}
	names := make([]string, 0, len(response.Headers))
	for name := range response.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		_ = answered.SetKey(starlark.String(name), starlark.String(response.Headers.Get(name)))
	}
	return primitiveResult(e, starlark.StringDict{
		"http_status_code": starlark.MakeInt(response.StatusCode),
		"headers":          answered,
		"body":             starlark.String(response.Body),
	}), nil
}

func (r *scriptRun) tcpExchange(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var address string
	textToSend, readUntilText := "", ""
	if e := starlark.UnpackArgs(b.Name(), args, kwargs, "address", &address, "text_to_send?", &textToSend, "read_until_text?", &readUntilText); e != nil {
		return nil, e
	}
	received, e := primitives.TCPExchange(r.ctx, address, textToSend, readUntilText)
	return primitiveResult(r.endedShortlyBeforeTheTimeLimit(e, "the call"), starlark.StringDict{"received": starlark.String(received)}), nil
}

func (r *scriptRun) udpExchange(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var address, textToSend string
	if e := starlark.UnpackArgs(b.Name(), args, kwargs, "address", &address, "text_to_send", &textToSend); e != nil {
		return nil, e
	}
	received, e := primitives.UDPExchange(r.ctx, address, textToSend)
	return primitiveResult(r.endedShortlyBeforeTheTimeLimit(e, "the call"), starlark.StringDict{"received": starlark.String(received)}), nil
}

// readState returns the value that write_state stored at an earlier run of
// the check, or None.
func (r *scriptRun) readState(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if e := starlark.UnpackArgs(b.Name(), args, kwargs); e != nil {
		return nil, e
	}
	if r.stateDirectory == "" {
		return nil, fmt.Errorf("%s: only a check keeps state between runs", b.Name())
	}
	stored, e := ReadModuleState(r.stateDirectory)
	if e != nil {
		return nil, fmt.Errorf("%s: %w", b.Name(), e)
	}
	return toStarlark(stored), nil
}

// writeState stores a value for the next run of the check.
func (r *scriptRun) writeState(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var value starlark.Value
	if e := starlark.UnpackArgs(b.Name(), args, kwargs, "value", &value); e != nil {
		return nil, e
	}
	if r.stateDirectory == "" {
		return nil, fmt.Errorf("%s: only a check keeps state between runs", b.Name())
	}
	plain, e := fromStarlark(value)
	if e != nil {
		return nil, fmt.Errorf("%s: %w", b.Name(), e)
	}
	if e := WriteModuleState(r.stateDirectory, plain); e != nil {
		return nil, fmt.Errorf("%s: %w", b.Name(), e)
	}
	return starlark.None, nil
}

// toStarlark turns a value of a JSON request into the value that a script
// gets: an object as a dictionary, a list as a list, and a number as an int
// when it is a whole number.
func toStarlark(value any) starlark.Value {
	switch v := value.(type) {
	case nil:
		return starlark.None
	case bool:
		return starlark.Bool(v)
	case string:
		return starlark.String(v)
	case json.Number:
		if n, e := v.Int64(); e == nil {
			return starlark.MakeInt64(n)
		}
		f, _ := v.Float64()
		return starlark.Float(f)
	case float64:
		return starlark.Float(v)
	case int:
		return starlark.MakeInt(v)
	case int64:
		return starlark.MakeInt64(v)
	case []any:
		items := make([]starlark.Value, len(v))
		for i, item := range v {
			items[i] = toStarlark(item)
		}
		return starlark.NewList(items)
	case map[string]any:
		dictionary := starlark.NewDict(len(v))
		for _, key := range Names(v) {
			_ = dictionary.SetKey(starlark.String(key), toStarlark(v[key]))
		}
		return dictionary
	case map[string]string:
		dictionary := starlark.NewDict(len(v))
		for _, key := range Names(v) {
			_ = dictionary.SetKey(starlark.String(key), starlark.String(v[key]))
		}
		return dictionary
	}
	return starlark.None
}

// fromStarlark turns a value of a script into a value that MMWatchdog writes as
// JSON. A result and a state hold None, True, False, numbers, strings,
// lists, and dictionaries with strings as keys.
func fromStarlark(value starlark.Value) (any, error) {
	switch v := value.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(v), nil
	case starlark.String:
		return string(v), nil
	case starlark.Int:
		n, fits := v.Int64()
		if !fits {
			return nil, fmt.Errorf("the whole number %s is too large", v.String())
		}
		return n, nil
	case starlark.Float:
		return float64(v), nil
	case *starlark.List:
		items := make([]any, v.Len())
		for i := range items {
			item, e := fromStarlark(v.Index(i))
			if e != nil {
				return nil, e
			}
			items[i] = item
		}
		return items, nil
	case starlark.Tuple:
		items := make([]any, len(v))
		for i := range items {
			item, e := fromStarlark(v[i]) //nolint:nilaway // UnpackArgs binds the required value before conversion; tuple indexes range over a slice of the same length.
			if e != nil {
				return nil, e
			}
			items[i] = item
		}
		return items, nil
	case *starlark.Dict:
		fields := map[string]any{}
		for _, item := range v.Items() {
			key, isString := starlark.AsString(item[0])
			if !isString {
				return nil, fmt.Errorf("dictionary key %s must be a string", item[0].String())
			}
			field, e := fromStarlark(item[1])
			if e != nil {
				return nil, e
			}
			fields[key] = field
		}
		return fields, nil
	}
	return nil, fmt.Errorf("cannot write a value of type %s as JSON; use None, True, False, a number, a string, a list, or a dictionary", value.Type())
}

// verifyConnection opens and closes the transport without sending an HTTP
// request. Neither a webhook's path nor its credentials enter an error.
func (r *scriptRun) verifyConnection(_ *starlark.Thread, builtin *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var address string
	if err := starlark.UnpackArgs(builtin.Name(), args, kwargs, "url", &address); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return primitiveResult(errors.New("the connection address must be an http or https URL with a host"), starlark.StringDict{}), nil
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
		if parsed.Scheme == "https" {
			port = "443"
		}
	}
	target := net.JoinHostPort(parsed.Hostname(), port)
	if parsed.Scheme == "https" {
		_, err = primitives.TLSCertificateExpiry(r.ctx, target, parsed.Hostname(), nil)
	} else {
		var connection net.Conn
		connection, err = primitives.ConnectTCP(r.ctx, target)
		if err == nil {
			connection.Close()
		}
	}
	if err != nil {
		err = errors.New("the connection or server certificate check failed")
		err = r.endedShortlyBeforeTheTimeLimit(err, "connection verification")
	}
	return primitiveResult(err, starlark.StringDict{}), nil
}
