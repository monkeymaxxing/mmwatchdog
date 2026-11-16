// A check module of a team's own that is an executable, written in Go. It
// does the same as the Starlark script in ../queued_jobs, and shows the JSON
// protocol that MMWatchdog speaks with an executable: one JSON object on standard
// input, one on standard output.
//
// A module's directory holds the executable under the name module_executable:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o module_executable queued_jobs_in_go.go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The module only measures: the check that uses it states the limit, such as
// fail_when_measurement_above: {queued_jobs: 500}.
func main() {
	var request struct {
		Protocol int `json:"protocol"`
		Inputs   struct {
			QueueFilePath string `json:"queue_file_path"`
		} `json:"inputs"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil || request.Protocol != 1 {
		fmt.Fprintln(os.Stderr, "invalid request")
		os.Exit(1)
	}
	b, err := os.ReadFile(request.Inputs.QueueFilePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	queuedJobs, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// A measurement is a plain number. Its unit is in module_description.yml.
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"protocol": 1, "status": "ok", "message": fmt.Sprintf("%d queued jobs", queuedJobs), "measurements": map[string]any{"queued_jobs": queuedJobs}}) // The response contains only encodable values; a failed stdout write leaves the parent without a valid result, which it rejects.
}
