package checksandalerts

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
	"mmwatchdog/internal/primitives"
)

// validateScriptInputs runs only the optional, pure input validation hook.
// Node access belongs to ValidateOnNode, so the primitives are unavailable.
func validateScriptInputs(module *InstalledModule, inputs map[string]any) *FieldError {
	path := filepath.Join(module.Directory, ModuleScriptFile)
	source, err := primitives.ReadFile(path, moduleScriptLimit)
	if err != nil {
		return &FieldError{"", err.Error()}
	}
	// Most scripts have no hook. Do not evaluate their top-level code.
	// Runtime validation checks malformed scripts and their entry point.
	hasHook, parseError := scriptDeclaresName(path, source, "validate_inputs")
	if parseError != nil || !hasHook {
		return nil
	}
	thread := &starlark.Thread{Name: "validate_inputs"}
	thread.SetMaxExecutionSteps(starlarkStepLimit)
	predeclared := (&scriptRun{ctx: context.Background()}).predeclared()
	for name := range predeclared {
		if name == "json" {
			continue
		}
		predeclared[name] = starlark.NewBuiltin(name, func(_ *starlark.Thread, builtin *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
			return nil, fmt.Errorf("%s cannot be called during input validation; validate_inputs checks input values only", builtin.Name())
		})
	}
	globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, path, source, predeclared)
	if err != nil {
		return &FieldError{"", scriptError(path, err).Error()}
	}
	hook, exists := globals["validate_inputs"]
	if !exists {
		return nil
	}
	function, ok := hook.(*starlark.Function)
	if !ok {
		return &FieldError{"", path + ": validate_inputs must be a function"}
	}
	answer, err := starlark.Call(thread, function, starlark.Tuple{toStarlark(ModuleInputs(inputs))}, nil)
	if err != nil {
		return &FieldError{"", scriptError(path, err).Error()}
	}
	if answer == starlark.None {
		return nil
	}
	message, ok := starlark.AsString(answer)
	if !ok {
		return &FieldError{"", path + ": validate_inputs must return None or an error text"}
	}
	input := ""
	if name, text, found := strings.Cut(message, ":"); found {
		for _, description := range module.Description.Inputs {
			if name == description.Name {
				input, message = name, strings.TrimSpace(text)
				break
			}
		}
	}
	return &FieldError{input, message}
}

// scriptDeclaresName finds an optional hook without evaluating top-level code.
func scriptDeclaresName(path string, source []byte, name string) (bool, error) {
	parsed, err := (&syntax.FileOptions{}).Parse(path, source, 0)
	if err != nil {
		return false, scriptError(path, err)
	}
	for _, statement := range parsed.Stmts {
		switch statement := statement.(type) {
		case *syntax.DefStmt:
			if statement.Name.Name == name {
				return true, nil
			}
		case *syntax.AssignStmt:
			if identifier, ok := statement.LHS.(*syntax.Ident); ok && identifier.Name == name {
				return true, nil
			}
		}
	}
	return false, nil
}
