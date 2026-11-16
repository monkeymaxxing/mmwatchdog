package checksandalerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// PgBackRest is the inputs of the pgbackrest check module, which asks
// pgBackRest about the backups of one stanza and measures how old the
// latest one is. A stanza is pgBackRest's name for the backups of one
// PostgreSQL cluster.
type PgBackRest struct {
	Stanza string `yaml:"stanza" json:"stanza"`
}

func (b *PgBackRest) Validate(InputValidation) *FieldError {
	if !PlainText(b.Stanza, 256) || b.Stanza == "" || strings.ContainsAny(b.Stanza, " ") {
		return &FieldError{"stanza", "expected the name of a pgBackRest stanza, one word, such as main"}
	}
	return nil
}

// pgBackRestStanza is what MMWatchdog reads of the answer of "pgbackrest
// info --output=json" for one stanza. pgBackRest lists only the backups
// that finished. A backup with error true found damaged pages in the
// database.
type pgBackRestStanza struct {
	Name   string `json:"name"`
	Status struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Lock    struct {
			Backup struct {
				Held bool `json:"held"`
			} `json:"backup"`
		} `json:"lock"`
	} `json:"status"`
	Repositories []struct {
		Key    int `json:"key"`
		Status struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"status"`
	} `json:"repo"`
	Backups []struct {
		Label     string `json:"label"`
		Type      string `json:"type"`
		Error     bool   `json:"error"`
		Timestamp struct {
			Stop int64 `json:"stop"`
		} `json:"timestamp"`
	} `json:"backup"`
}

func (b *PgBackRest) Run(ctx context.Context, r *CheckRun) Observation {
	// pgBackRest runs as the account of the PostgreSQL server, which can
	// read pgBackRest's settings and the keys of its repository in them.
	ran, err := primitives.RunCommand(ctx, primitives.Command{Arguments: []string{"pgbackrest", "--stanza=" + b.Stanza, "--output=json", "info"}, RunAs: &primitives.ExecOptions{RunAsUserAccount: primitives.PostgreSQLAdministratorAccount, RunFromDirectory: "/"}, StandardOutputLimit: primitives.OwnStandardOutputLimit})
	var notFound *exec.Error
	switch {
	case errors.As(err, &notFound):
		return Result("unknown", primitives.DetailLine("MMWatchdog cannot run pgbackrest: "+notFound.Err.Error()+". The check needs that program on this node"))
	case err != nil && ctx.Err() != nil:
		return Result("failed", "pgBackRest did not answer within the time limit of the run")
	case err != nil:
		return Result("unknown", primitives.DetailLine("MMWatchdog cannot run pgbackrest as the account "+primitives.PostgreSQLAdministratorAccount+": "+err.Error()))
	case ran.ExitCode != 0:
		said := strings.TrimSpace(ran.StandardError + "\n" + ran.StandardOutput)
		if said == "" {
			said = "pgbackrest ended with exit code " + strconv.Itoa(ran.ExitCode)
		}
		return Result("failed", "pgBackRest did not answer the check: "+clientLine(whatTheClientSays(said)))
	}
	var stanzas []pgBackRestStanza
	if json.Unmarshal([]byte(ran.StandardOutput), &stanzas) != nil || len(stanzas) != 1 || stanzas[0].Name != b.Stanza || stanzas[0].Status.Code == nil {
		return Result("unknown", primitives.DetailLine("pgbackrest gave an answer that MMWatchdog does not understand: "+primitives.Clip(ran.StandardOutput, 200)))
	}
	stanza := stanzas[0]
	// pgBackRest exits with 0 also when the stanza has a problem, and says
	// it in the stanza's status. A problem of a repository has its reason in
	// the repository's status, and the stanza's says only "other".
	if *stanza.Status.Code != 0 {
		reasons := []string{}
		for _, repository := range stanza.Repositories {
			if repository.Status.Code != 0 {
				reasons = append(reasons, fmt.Sprintf("repo%d: %s", repository.Key, firstLine(repository.Status.Message)))
			}
		}
		if len(reasons) == 0 {
			reasons = append(reasons, firstLine(stanza.Status.Message))
		}
		return Result("failed", primitives.DetailLine("pgBackRest reports a problem with stanza "+b.Stanza+": "+strings.Join(reasons, "; ")))
	}
	// The latest backup counts only without errors. A newer backup that
	// found damaged pages is named in the message.
	latest, damaged := -1, []string{}
	for i, backup := range stanza.Backups {
		switch {
		case backup.Error:
			damaged = append(damaged, backup.Label)
		case latest < 0 || backup.Timestamp.Stop > stanza.Backups[latest].Timestamp.Stop:
			latest = i
		}
	}
	running := ""
	if stanza.Status.Lock.Backup.Held {
		running = "; a backup is running now"
	}
	if len(stanza.Backups) == 0 {
		return Result("failed", primitives.DetailLine("stanza "+b.Stanza+" has no backup"+running))
	}
	if latest < 0 {
		return Result("failed", primitives.DetailLine("stanza "+b.Stanza+" has no backup without errors; pgBackRest found damaged pages in "+strings.Join(damaged, ", ")+running))
	}
	backup := stanza.Backups[latest]
	newerDamaged := []string{}
	for _, label := range damaged {
		for _, other := range stanza.Backups {
			if other.Label == label && other.Timestamp.Stop > backup.Timestamp.Stop {
				newerDamaged = append(newerDamaged, label)
			}
		}
	}
	hours := max(time.Since(time.Unix(backup.Timestamp.Stop, 0)).Hours(), 0)
	o := Result("ok", primitives.DetailLine(fmt.Sprintf("the latest backup of stanza %s finished %s hours ago: %s, a %s backup", b.Stanza, primitives.FormatReading(hours), backup.Label, backup.Type)))
	if len(newerDamaged) > 0 {
		o.Message += "; pgBackRest found damaged pages in the newer " + strings.Join(newerDamaged, ", ")
	}
	o.Message += running
	o.Measurements = map[string]Measurement{"latest_backup_age_hours": {hours, "hours"}}
	return withLimits(r.Check, o, nil)
}

// firstLine is the first line of a message of pgBackRest, which puts the
// reason first and its details and hints on the lines below.
func firstLine(message string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	return line
}

// pgbackrestDescription is what "mmwatchdog describe-module pgbackrest"
// shows, and what validation knows about the module's inputs.
var pgbackrestDescription = ModuleDescription{
	Summary: "Asks pgBackRest about the backups of one stanza, and measures how many hours ago its latest backup without errors finished. The check fails when pgBackRest reports a problem with the stanza or its repository.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "stanza", Type: "string", Required: true, Description: "The pgBackRest stanza: pgBackRest's name for the backups of one PostgreSQL cluster, such as main."},
	},
	Measurements: []ModuleMeasurement{
		{Name: "latest_backup_age_hours", Unit: "hours", Description: "The hours since the latest backup of the stanza without errors finished. A backup of every type counts: full, differential, and incremental."},
	},
	Examples: []ModuleExample{
		{
			Description:   "The latest backup of the stanza main finished less than 26 hours ago, so a daily backup has not been missed.",
			Configuration: "pgbackrest:\n  stanza: main\nfail_when_measurement_above:\n  latest_backup_age_hours: 26\nalert_severity: warn\n",
		},
	},
}
