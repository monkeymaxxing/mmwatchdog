package checksandalerts

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// AptSecurityUpdates is the inputs of the apt_security_updates check
// module, which fails while a security update waits to be installed on the
// node. The check reads what apt already knows and downloads nothing.
type AptSecurityUpdates struct {
	FailWhen string `yaml:"fail_when" json:"fail_when"`
}

func (n *AptSecurityUpdates) Validate(v InputValidation) *FieldError {
	inputs := map[string]any{}
	if n.FailWhen != "" || v.Authored != nil && v.Authored("fail_when") {
		inputs["fail_when"] = n.FailWhen
	}
	return aptSecurityUpdatesDescription.InputsProblem("apt_security_updates", inputs)
}

// packageListsTooOldAfter is how old the package manager's lists of
// available packages may be. With older lists, the package manager does not
// know the updates of the last days, and "no update waits" would be a guess.
const packageListsTooOldAfter = 7 * 24 * time.Hour

func (n *AptSecurityUpdates) Run(ctx context.Context, _ *CheckRun) Observation {
	family, name := primitives.NodeDistributionFamily()
	name = namedOrUnnamed(name)
	switch family {
	case primitives.DebianFamily:
	case primitives.RedHatFamily:
		return Result("unknown", "this node runs "+name+", whose package manager is dnf, not apt: use the check dnf_security_updates")
	case primitives.SUSEFamily:
		return Result("unknown", "this node runs "+name+", whose package manager is zypper, not apt: use the check zypper_security_patches")
	default:
		return unknownPackageManager(name)
	}
	updates, problem := securityUpdatesOfAPT(ctx)
	if problem != nil {
		return *problem
	}
	if len(updates) == 0 {
		return Result("ok", "no security update waits to be installed")
	}
	updates = uniqueSorted(updates)
	return Result("failed", countOf(len(updates), "security update waits", "security updates wait")+" to be installed: "+shownNames(updates))
}

// unknownPackageManager is the observation of a node whose distribution is
// in none of the families that have a check of security updates. The three
// checks of security updates say the same.
func unknownPackageManager(name string) Observation {
	return Result("unknown", "MMWatchdog does not know the package manager of "+name+": it knows only that of Debian and Ubuntu (apt_security_updates), that of the Red Hat family (dnf_security_updates), and that of SUSE (zypper_security_patches)")
}

// namedOrUnnamed is the distribution's PRETTY_NAME for a message, or words
// that say /etc/os-release names none.
func namedOrUnnamed(prettyName string) string {
	if prettyName == "" {
		return "a distribution that /etc/os-release does not name"
	}
	return prettyName
}

// releaseOfTheNode names the node's release at the start of an exact
// finding of a package check: ID and VERSION_ID of /etc/os-release, such as
// "debian 13" or "rocky 9.4". Two nodes of different releases can have the
// same versions of a package and still need different fixes.
func releaseOfTheNode() string {
	release := primitives.NodeOSRelease()
	return strings.TrimSpace(release["ID"] + " " + release["VERSION_ID"])
}

// hasNonemptyPackageLists requires actual package data, not a cache directory
// or repository index. List timestamps cannot prove a successful refresh.
func hasNonemptyPackageLists(paths ...string) bool {
	for _, pattern := range paths {
		found, _ := filepath.Glob(primitives.NodeFile(pattern))
		for _, path := range found {
			if about, err := primitives.InformationAboutFile(path); err == nil && about.IsRegularFile && about.SizeBytes > 0 {
				return true
			}
		}
	}
	return false
}

// packageListsProblem requires both package data and a recent successful
// refresh. A partial refresh can leave a fresh list beside stale security data.
func packageListsProblem(manager, refresh string, listsExist bool, successStamp string) *Observation {
	if !listsExist {
		o := Result("unknown", manager+" has no lists of available packages on this node, so MMWatchdog cannot know which updates wait: "+refresh)
		return &o
	}
	about, err := primitives.InformationAboutFile(primitives.NodeFile(successStamp))
	if err != nil || !about.IsRegularFile {
		o := Result("unknown", manager+" has no usable successful-refresh stamp at "+successStamp+", so MMWatchdog cannot know which updates wait: "+refresh)
		return &o
	}
	age := time.Since(about.Modified)
	if age > packageListsTooOldAfter {
		evidence := fmt.Sprintf("the stamp %s, which %s writes after a successful refresh, is %d days old", successStamp, manager, int(age.Hours()/24))
		o := Result("unknown", evidence+", so MMWatchdog cannot know which updates wait: "+refresh)
		return &o
	}
	return nil
}

// aptRefresh points to how apt comes to write the success stamp; the
// configuration itself lives on the module page, not in every message. apt runs a
// Post-Invoke-Success hook only after a refresh in which every list
// arrived. Ubuntu's update-notifier-common installs that hook; Debian has
// no package with it: its apt-config-auto-update touches the stamp with
// Post-Invoke, after every refresh, also a failed one.
const aptRefresh = `have apt write this stamp after each successful refresh; the module page apt_security_updates, section "Set up", shows how (mmwatchdog describe-module apt_security_updates)`

// aptInstallPattern finds, in what "apt-get --simulate" writes, a package
// that an upgrade would install, and where its new version comes from: a
// line such as "Inst libssl3 [3.0.11-1] (3.0.13-1 Debian-Security:12/stable-security [amd64])".
var aptInstallPattern = regexp.MustCompile(`^Inst (\S+) (?:\[[^\]]*\] )?\(([^)]*)\)`)

// securityUpdatesOfAPT asks apt which packages an upgrade would install
// from a security archive. apt only simulates the upgrade, as the
// unprivileged account mmwatchdog, which can change nothing.
func securityUpdatesOfAPT(ctx context.Context) ([]string, *Observation) {
	// A failed refresh can touch lists/partial or update-stamp. Only a
	// successful-refresh stamp proves freshness, and package data must exist.
	lists := hasNonemptyPackageLists("/var/lib/apt/lists/*_Packages", "/var/lib/apt/lists/*_Packages.*")
	if old := packageListsProblem("apt", aptRefresh, lists, "/var/lib/apt/periodic/update-success-stamp"); old != nil {
		return nil, old
	}
	ran, err := primitives.RunCommand(ctx, primitives.Command{Arguments: []string{"apt-get", "--simulate", "--quiet", "dist-upgrade"}, RunAs: &primitives.ExecOptions{RunAsUserAccount: primitives.UnprivilegedAccount, RunFromDirectory: "/", Env: map[string]string{"LC_ALL": "C"}}, StandardOutputLimit: primitives.OwnStandardOutputLimit})
	if problem := packageManagerProblem("apt-get", ran, err); problem != nil {
		return nil, problem
	}
	return securityUpdatesInAPTSimulation(ran.StandardOutput), nil
}

// securityUpdatesInAPTSimulation reads the packages that come from a
// security archive from what "apt-get --simulate dist-upgrade" wrote.
// Debian names its archive Debian-Security, and Ubuntu's archives end with
// -security.
func securityUpdatesInAPTSimulation(written string) []string {
	updates := []string{}
	for _, line := range strings.Split(written, "\n") {
		if found := aptInstallPattern.FindStringSubmatch(line); found != nil && strings.Contains(strings.ToLower(found[2]), "security") {
			updates = append(updates, found[1])
		}
	}
	return updates
}

// packageManagerProblem is the observation of a package manager that could
// not answer: the check is unknown.
func packageManagerProblem(program string, ran primitives.CommandResult, err error) *Observation {
	var notFound *exec.Error
	message := ""
	switch {
	case errors.As(err, &notFound):
		message = "MMWatchdog cannot run " + program + ": " + notFound.Err.Error()
	case err != nil:
		message = "MMWatchdog cannot ask " + program + " which updates wait: " + err.Error()
	case ran.ExitCode != 0:
		message = "MMWatchdog cannot ask " + program + " which updates wait: " + primitives.Clip(strings.TrimSpace(ran.StandardError), 300)
	default:
		return nil
	}
	o := Result("unknown", primitives.DetailLine(message))
	return &o
}

// aptSecurityUpdatesDescription is what "mmwatchdog describe-module apt_security_updates" shows, and what
// validation knows about the module's inputs.
var aptSecurityUpdatesDescription = ModuleDescription{
	Summary: "Fails while a security update waits to be installed on the node. The check reads what the node's own package manager already knows, and downloads nothing.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "fail_when", Type: "string", Required: true, AllowedValues: []any{"a_security_update_waits_to_be_installed"}, Description: "The check fails while a security update waits to be installed; write a_security_update_waits_to_be_installed, the only value."},
	},
	Examples: []ModuleExample{{
		Description:   "Tell the team when a security update waits to be installed.",
		Configuration: "apt_security_updates:\n  fail_when: a_security_update_waits_to_be_installed\nalert_severity: warn\n",
	}},
}
