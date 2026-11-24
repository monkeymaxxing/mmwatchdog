package checksandalerts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"mmwatchdog/internal/primitives"
)

// HelperError distinguishes command arguments from invalid module data.
type HelperError struct {
	Code int
	Err  error
}

func (e *HelperError) Error() string { return e.Err.Error() }
func (e *HelperError) Unwrap() error { return e.Err }

// ModuleJSONHelper runs independently of configuration or the daemon.
func ModuleJSONHelper(args []string, in io.Reader, out io.Writer) (err error) {
	defer func() {
		if err != nil {
			if _, ok := err.(*HelperError); !ok {
				err = &HelperError{1, err}
			}
		}
	}()
	badArgs := func(message string) error { return &HelperError{4, fmt.Errorf("%s", message)} }
	read := func() ([]byte, error) {
		b, e := io.ReadAll(io.LimitReader(in, ModuleRequestLimit+1))
		if e != nil {
			return nil, e
		}
		if len(b) > ModuleRequestLimit {
			return nil, fmt.Errorf("input exceeds %d bytes", ModuleRequestLimit)
		}
		return b, nil
	}
	if len(args) == 0 {
		return badArgs("usage: mmwatchdog module-json request|get|result|object")
	}
	switch args[0] {
	case "request":
		var allowed map[string]bool
		if len(args) != 1 {
			if len(args) != 3 || args[1] != "--inputs" {
				return badArgs("usage: mmwatchdog module-json request [--inputs NAME,...]")
			}
			allowed = map[string]bool{}
			if args[2] != "" {
				for _, name := range strings.Split(args[2], ",") {
					if name == "" || allowed[name] {
						return badArgs("--inputs requires distinct nonempty input names")
					}
					allowed[name] = true
				}
			}
		}
		b, e := read()
		if e != nil {
			return e
		}
		// The request of a check's module also holds the state directory of
		// the check. The request of a destination module also holds the
		// alert and the content of the files that its inputs name. Both say
		// when MMWatchdog ends the module.
		var request struct {
			Protocol                                 int                        `json:"protocol"`
			Inputs                                   map[string]json.RawMessage `json:"inputs"`
			StateDirectory                           string                     `json:"state_dir"`
			Alert                                    map[string]json.RawMessage `json:"alert"`
			FileContents                             map[string]string          `json:"file_contents"`
			MMWatchdogEndsTheModuleAfterMilliseconds *int64                     `json:"mmwatchdog_ends_the_module_after_milliseconds"`
		}
		if e = DecodeJSONUpTo(b, &request, ModuleRequestLimit); e != nil {
			return e
		}
		if request.Protocol != 1 {
			return fmt.Errorf("unsupported module request protocol %d; expected protocol: 1", request.Protocol)
		}
		if request.Inputs == nil {
			return fmt.Errorf("inputs must be an object")
		}
		for _, name := range Names(request.Inputs) {
			if allowed != nil && !allowed[name] {
				return fmt.Errorf("unknown input %q", name)
			}
		}
		_, e = out.Write(b)
		return e
	case "get":
		kind := "json"
		rest := args[1:]
		if len(rest) >= 2 && rest[0] == "--type" {
			kind, rest = rest[1], rest[2:]
		}
		if len(rest) != 1 {
			return badArgs("usage: mmwatchdog module-json get [--type TYPE] POINTER")
		}
		switch kind {
		case "string", "integer", "number", "boolean", "array", "object", "null", "json":
		default:
			return badArgs("unknown value type " + strconv.Quote(kind))
		}
		pointer := rest[0]
		if pointer != "" && !strings.HasPrefix(pointer, "/") {
			return badArgs("JSON Pointer must be empty or start with /")
		}
		var segments []string
		if pointer != "" {
			for _, segment := range strings.Split(pointer[1:], "/") {
				var decoded strings.Builder
				for i := 0; i < len(segment); i++ {
					if segment[i] != '~' {
						decoded.WriteByte(segment[i])
						continue
					}
					i++
					if i >= len(segment) || (segment[i] != '0' && segment[i] != '1') {
						return badArgs("invalid JSON Pointer escape")
					}
					if segment[i] == '0' {
						decoded.WriteByte('~')
					} else {
						decoded.WriteByte('/')
					}
				}
				segments = append(segments, decoded.String())
			}
		}
		b, e := read()
		if e != nil {
			return e
		}
		var value any
		if e = DecodeJSONUpTo(b, &value, ModuleRequestLimit); e != nil {
			return e
		}
		for _, segment := range segments {
			switch current := value.(type) {
			case map[string]any:
				var exists bool
				value, exists = current[segment]
				if !exists {
					return fmt.Errorf("missing value at JSON Pointer %q", pointer)
				}
			case []any:
				index, e := strconv.Atoi(segment)
				if e != nil || index < 0 || index >= len(current) || strconv.Itoa(index) != segment {
					return fmt.Errorf("missing array element at JSON Pointer %q", pointer)
				}
				value = current[index]
			default:
				return fmt.Errorf("missing value at JSON Pointer %q", pointer)
			}
		}
		matches := kind == "json"
		switch kind {
		case "string":
			_, matches = value.(string)
		case "number", "integer":
			_, matches = value.(json.Number)
		case "boolean":
			_, matches = value.(bool)
		case "array":
			_, matches = value.([]any)
		case "object":
			_, matches = value.(map[string]any)
		case "null":
			matches = value == nil
		}
		if !matches {
			return fmt.Errorf("value at %q: expected %s", pointer, kind)
		}
		if kind == "integer" {
			n, ok := wholeNumber(string(value.(json.Number)))
			if !ok || !n.IsInt() || !n.Num().IsInt64() {
				return fmt.Errorf("value at %q: expected signed 64-bit integer", pointer)
			}
			_, e = fmt.Fprintln(out, n.Num().String())
			return e
		}
		if s, ok := value.(string); ok && kind == "string" {
			if strings.ContainsRune(s, 0) {
				return fmt.Errorf("string contains NUL, which Bash cannot represent")
			}
			_, e = io.WriteString(out, s)
			return e
		}
		return json.NewEncoder(out).Encode(value)
	case "result":
		result := ModuleResult{Protocol: 1}
		messageSet, stdinMessage, statusSet := false, false, false
		exactFindings := map[string]string{}
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--status", "--message":
				option := args[i]
				i++
				if i >= len(args) {
					return badArgs(option + " requires a value")
				}
				if option == "--status" {
					if statusSet {
						return badArgs("--status may be supplied once")
					}
					statusSet = true
					result.Status = args[i]
				} else {
					if messageSet || stdinMessage {
						return badArgs("use exactly one of --message or --message-stdin")
					}
					messageSet = true
					result.Message = args[i]
				}
			case "--message-stdin":
				if messageSet || stdinMessage {
					return badArgs("use exactly one of --message or --message-stdin")
				}
				stdinMessage = true
			case "--measurement":
				if i+2 >= len(args) {
					return badArgs("--measurement requires NAME VALUE")
				}
				name, text := args[i+1], args[i+2]
				i += 2
				if result.Measurements == nil {
					result.Measurements = map[string]float64{}
				}
				if _, exists := result.Measurements[name]; exists {
					return fmt.Errorf("duplicate measurement %q", name)
				}
				value, e := strconv.ParseFloat(text, 64)
				if e != nil {
					return fmt.Errorf("measurement %s: expected a number", name)
				}
				result.Measurements[name] = value
			case "--finding":
				if i+2 >= len(args) {
					return badArgs("--finding requires ID MESSAGE")
				}
				result.Findings = append(result.Findings, Finding{ID: args[i+1], Message: args[i+2]})
				i += 2
			case "--exact-finding":
				if i+2 >= len(args) {
					return badArgs("--exact-finding requires ID TEXT")
				}
				if _, given := exactFindings[args[i+1]]; given {
					return fmt.Errorf("--exact-finding %s may be supplied once", strconv.Quote(args[i+1]))
				}
				exactFindings[args[i+1]] = args[i+2]
				i += 2
			default:
				return badArgs("unknown result argument " + strconv.Quote(args[i]))
			}
		}
		// Each exact finding belongs to the finding that its ID names, which
		// may come before or after it.
		for index, finding := range result.Findings {
			if text, given := exactFindings[finding.ID]; given {
				result.Findings[index].ExactFinding = text
				delete(exactFindings, finding.ID)
			}
		}
		if len(exactFindings) > 0 {
			return fmt.Errorf("--exact-finding %s names no --finding", strconv.Quote(Names(exactFindings)[0]))
		}
		if !statusSet || (!messageSet && !stdinMessage) {
			return badArgs("result requires --status and exactly one of --message or --message-stdin")
		}
		switch result.Status {
		case "ok", "failed", "unknown":
		case deliveredStatus:
			if len(result.Measurements) > 0 || len(result.Findings) > 0 {
				return badArgs("--status delivered is the answer of a destination module and takes no --measurement and no --finding")
			}
		default:
			return badArgs("--status must be ok, failed, or unknown for a check, and delivered or failed for an alert destination")
		}
		if stdinMessage {
			b, e := read()
			if e != nil {
				return e
			}
			result.Message = string(b)
		}
		if result.Status == deliveredStatus {
			if result.Message == "" {
				return fmt.Errorf("a message is required")
			}
		} else if e := ValidateResult(result); e != nil {
			return e
		}
		b, e := json.Marshal(result)
		if e != nil {
			return e
		}
		if len(b) > primitives.ResultLimit {
			return fmt.Errorf("module result exceeds the 64 KiB JSON output limit")
		}
		_, e = fmt.Fprintln(out, string(b))
		return e
	case "object":
		// The members are written in the order given, each value as JSON.
		type member struct {
			name  string
			value []byte
		}
		var members []member
		seen := map[string]bool{}
		stdinMember := -1
		for i := 1; i < len(args); i++ {
			option := args[i]
			values := map[string]int{"--string": 1, "--json": 1, "--string-stdin": 0}
			if _, known := values[option]; !known {
				return badArgs("unknown object argument " + strconv.Quote(option))
			}
			if i+1+values[option] >= len(args) {
				if option == "--string-stdin" {
					return badArgs("--string-stdin requires NAME")
				}
				return badArgs(option + " requires NAME and a value")
			}
			m := member{name: args[i+1]}
			if seen[m.name] {
				return fmt.Errorf("duplicate member %q", m.name)
			}
			seen[m.name] = true
			var e error
			switch option {
			case "--string":
				m.value, e = jsonString(args[i+2])
			case "--json":
				m.value = bytes.TrimSpace([]byte(args[i+2]))
				e = oneJSONValue(m.value)
			case "--string-stdin":
				if stdinMember >= 0 {
					return badArgs("--string-stdin may be supplied once")
				}
				stdinMember = len(members)
			}
			if e != nil {
				return fmt.Errorf("member %s: %w", m.name, e)
			}
			members = append(members, m)
			i += 1 + values[option]
		}
		if stdinMember >= 0 {
			b, e := read()
			if e != nil {
				return e
			}
			//nolint:nilaway // stdinMember records an index whose member is appended before the lookup; earlier failures return.
			if members[stdinMember].value, e = jsonString(string(b)); e != nil {
				return fmt.Errorf("member %s: %w", members[stdinMember].name, e)
			}
		}
		var object bytes.Buffer
		object.WriteByte('{')
		for i, m := range members {
			if i > 0 {
				object.WriteByte(',')
			}
			name, e := jsonString(m.name)
			if e != nil {
				return fmt.Errorf("member name: %w", e)
			}
			object.Write(name)
			object.WriteByte(':')
			object.Write(m.value)
		}
		object.WriteString("}\n")
		_, e := out.Write(object.Bytes())
		return e
	default:
		return badArgs("unknown module-json command " + strconv.Quote(args[0]))
	}
}

// deliveredStatus is the status with which a destination module answers
// that its service accepted the alert.
const deliveredStatus = "delivered"

// jsonString writes text as a JSON string, with the characters <, >, and &
// left as they are.
func jsonString(text string) ([]byte, error) {
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("the text is not valid UTF-8")
	}
	return PlainJSON(text)
}

// oneJSONValue accepts exactly one JSON value of any type, without a
// duplicate key in any object.
func oneJSONValue(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if e := uniqueJSON(d); e != nil {
		return fmt.Errorf("expected one JSON value: %w", e)
	}
	if _, e := d.Token(); e != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

// Bound exponents before allocating a rational. A nonzero int64 cannot need
// more decimal shifts than the input digits plus the int64 digit count.
func wholeNumber(text string) (*big.Rat, bool) {
	mantissa, exponent, hasExponent := strings.Cut(strings.ToLower(text), "e")
	if strings.Trim(mantissa, "-0.") == "" {
		return new(big.Rat), true
	}
	if hasExponent {
		shift, err := strconv.Atoi(exponent)
		if err != nil || shift > len(mantissa)+19 || shift < -len(mantissa)-19 {
			return nil, false
		}
	}
	return new(big.Rat).SetString(text)
}
