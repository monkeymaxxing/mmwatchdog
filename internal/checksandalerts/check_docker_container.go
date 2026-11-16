package checksandalerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"mmwatchdog/internal/primitives"
)

// DockerContainer is the inputs of the docker_container check module: the
// name of the container that must run. The check asks Docker through its
// own program. Restarting a container that ended is the job of Docker's
// restart policy, as restarting a program is the job of systemd.
type DockerContainer string

// dockerContainerNamePattern accepts the name of a container as Docker
// accepts one.
var dockerContainerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)

func (d *DockerContainer) Validate(InputValidation) *FieldError {
	if !dockerContainerNamePattern.MatchString(string(*d)) {
		return &FieldError{"", "must be the name of a Docker container, such as myapp-web"}
	}
	return nil
}

// dockerContainerState is what "docker inspect" says about the state of a
// container.
type dockerContainerState struct {
	Status  string `json:"Status"`
	Running bool   `json:"Running"`
	// Paused is true for a container whose processes Docker has frozen.
	// Docker reports such a container as running too.
	Paused     bool `json:"Paused"`
	Restarting bool `json:"Restarting"`
	ExitCode   int  `json:"ExitCode"`
	// Health is there for a container that declares a health check.
	Health *struct {
		Status string `json:"Status"`
		Log    []struct {
			Output string `json:"Output"`
		} `json:"Log"`
	} `json:"Health"`
}

func (d *DockerContainer) Run(ctx context.Context, _ *CheckRun) Observation {
	name := string(*d)
	written, err := primitives.RunSystemProgram(ctx, "docker", "container", "inspect", "--format", "{{json .State}}", name)
	var notFound *exec.Error
	switch {
	case errors.As(err, &notFound):
		return Result("unknown", "MMWatchdog cannot run docker: "+notFound.Err.Error())
	case err != nil && strings.Contains(strings.ToLower(err.Error()), "no such container"), err != nil && strings.Contains(strings.ToLower(err.Error()), "no such object"):
		// A container that was removed does not run.
		return Result("failed", "Docker has no container named "+name)
	case err != nil:
		return Result("unknown", primitives.DetailLine("MMWatchdog cannot ask Docker about the container "+name+": "+err.Error()))
	}
	var state dockerContainerState
	if err := json.Unmarshal([]byte(written), &state); err != nil {
		return Result("unknown", "docker gave an answer about the container "+name+" that MMWatchdog does not understand")
	}
	switch {
	case state.Restarting:
		return Result("failed", fmt.Sprintf("the Docker container %s is restarting again and again; it last ended with exit code %d", name, state.ExitCode))
	case state.Paused:
		return Result("failed", "the Docker container "+name+" is paused: its programs are stopped where they were, and do no work")
	case !state.Running && state.Status == "exited":
		return Result("failed", fmt.Sprintf("the Docker container %s does not run: it ended with exit code %d", name, state.ExitCode))
	case !state.Running:
		return Result("failed", primitives.DetailLine("the Docker container "+name+" does not run: Docker reports it as "+state.Status))
	case state.Health != nil && state.Health.Status == "unhealthy":
		// A container whose own health check fails is running and not
		// working, which Docker itself says.
		o := Result("failed", "the Docker container "+name+" runs, and its own health check fails")
		if log := state.Health.Log; len(log) > 0 && strings.TrimSpace(log[len(log)-1].Output) != "" {
			o.Details = []string{"the health check wrote: " + primitives.DetailLine(primitives.Clip(log[len(log)-1].Output, primitives.DetailLineLimit-30))}
		}
		return o
	case state.Health != nil && state.Health.Status == "healthy":
		return Result("ok", "the Docker container "+name+" runs, and its own health check passes")
	case state.Health != nil:
		return Result("ok", "the Docker container "+name+" runs, and its own health check has no result yet")
	}
	return Result("ok", "the Docker container "+name+" runs")
}

// dockerContainerDescription is what "mmwatchdog describe-module docker_container" shows, and what
// validation knows about the module's inputs.
var dockerContainerDescription = ModuleDescription{
	Summary: "Asks Docker whether a named container runs. A container whose own health check fails counts as failed too.",
	UsedAs:  UsedAsCheck,
	Value:   &ModuleInput{Type: "string", Required: true, Description: "The name of the Docker container that must run."},
	Examples: []ModuleExample{{
		Description:   "The container of the web application runs.",
		Configuration: "docker_container: myapp-web\nalert_severity: sos\n",
	}},
}
