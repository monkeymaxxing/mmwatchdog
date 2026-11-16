package checksandalerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"slices"
	"strings"

	"mmwatchdog/internal/primitives"
)

// RebootAfterUpdates is the inputs of the reboot_after_updates check
// module, which reads /run/reboot-required on Debian and Ubuntu,
// /run/reboot-needed on SUSE, and asks dnf on the Red Hat family, to find
// whether the node must reboot to use installed updates.
type RebootAfterUpdates struct {
	FailWhen string `yaml:"fail_when" json:"fail_when"`
}

func (n *RebootAfterUpdates) Validate(v InputValidation) *FieldError {
	inputs := map[string]any{}
	if n.FailWhen != "" || v.Authored != nil && v.Authored("fail_when") {
		inputs["fail_when"] = n.FailWhen
	}
	return rebootAfterUpdatesDescription.InputsProblem("reboot_after_updates", inputs)
}

// kernelHooksThatWriteRebootRequired are the programs of Debian and Ubuntu
// that write /run/reboot-required when a new kernel is installed. The
// kernel's package runs the hooks in /etc/kernel/postinst.d, and on newer
// releases also those in /usr/share/kernel/postinst.d. On a node without
// one of them, a missing file says nothing: Debian 12 and 13 have one only
// with unattended-upgrades or reboot-notifier. Other packages, such as dbus,
// write the file only for a new version of themselves.
var kernelHooksThatWriteRebootRequired = []string{
	"/etc/kernel/postinst.d/unattended-upgrades",         // unattended-upgrades, on Debian and Ubuntu
	"/etc/kernel/postinst.d/update-notifier",             // update-notifier-common, on Ubuntu
	"/etc/kernel/postinst.d/reboot-notifier",             // reboot-notifier, on Debian and Ubuntu
	"/usr/share/kernel/postinst.d/touch-reboot-required", // linux-base from version 4.13: Ubuntu 26.04, and Debian after 13
}

func (n *RebootAfterUpdates) Run(ctx context.Context, _ *CheckRun) Observation {
	family, name := primitives.NodeDistributionFamily()
	switch family {
	case primitives.DebianFamily:
		return rebootRequiredOnDebianOrUbuntu()
	case primitives.SUSEFamily:
		return rebootNeededOnSUSE()
	case primitives.RedHatFamily:
		return dnfOfTheNode(ctx).rebootHint(ctx)
	}
	return Result("unknown", "MMWatchdog does not know how "+namedOrUnnamed(name)+" says that a node needs a reboot: it knows only Debian, Ubuntu, the Red Hat family, and SUSE")
}

const nodeNeedsNoReboot = "this node needs no reboot"

// rebootNeededOnSUSE reads /run/reboot-needed. zypper creates the empty
// file when a transaction installs a package with a reboot hint, such as a
// kernel, and a reboot clears it, because /run is a tmpfs. zypper itself
// writes it, so a missing file always means that no such package was
// installed since the boot.
func rebootNeededOnSUSE() Observation {
	if _, err := primitives.InformationAboutFile(primitives.NodeFile("/run/reboot-needed")); errors.Is(err, fs.ErrNotExist) {
		return Result("ok", nodeNeedsNoReboot)
	} else if err != nil {
		return Result("unknown", "MMWatchdog cannot look at /run/reboot-needed: "+err.Error())
	}
	return Result("failed", "this node needs a reboot (/run/reboot-needed exists)")
}

// rebootHintOfDNF asks dnf. dnf's command needs-restarting ends with exit
// code 1 when the node needs a reboot, and says why on its standard output.
// It only reads, so it runs as the unprivileged account. Exit code 1 is also
// the code of a program that failed: without the reasons, MMWatchdog knows
// nothing.
//
// The command comes with dnf's plugins, the package dnf-plugins-core, which
// Rocky Linux and AlmaLinux do not install. The program needs-restarting of
// the package yum-utils needs that package and only runs the same command
// of dnf. The question needs only the installed packages, so dnf gets no
// repository: dnf would otherwise fetch the lists of every repository for
// the unprivileged account, which takes many seconds and reaches other
// machines.
func rebootHintOfDNF(ctx context.Context) Observation {
	ran, problem := runDNFNeedsRestarting(ctx, "--reboothint")
	switch {
	case problem != nil:
		return *problem
	case ran.ExitCode == 0:
		return Result("ok", nodeNeedsNoReboot)
	case ran.ExitCode == 1 && strings.TrimSpace(ran.StandardOutput) != "":
		o := Result("failed", "this node needs a reboot (dnf needs-restarting --reboothint)")
		for _, line := range strings.Split(ran.StandardOutput, "\n") {
			if line = strings.TrimSpace(line); line != "" && len(o.Details) < primitives.DetailLinesLimit {
				o.Details = append(o.Details, primitives.DetailLine(line))
			}
		}
		return o
	case strings.Contains(ran.StandardError, "No such command"):
		return Result("unknown", "the dnf of this node has no command needs-restarting, which says whether the node needs a reboot: install the package of dnf's plugins, with dnf install dnf-plugins-core")
	}
	return Result("unknown", primitives.DetailLine(fmt.Sprintf("dnf needs-restarting ended with exit code %d and no reason for a reboot: %s", ran.ExitCode, primitives.Clip(strings.TrimSpace(ran.StandardError), 200))))
}

// runDNFNeedsRestarting runs dnf's command needs-restarting with the given
// option. The observation is not nil when dnf could not run at all.
func runDNFNeedsRestarting(ctx context.Context, option string) (primitives.CommandResult, *Observation) {
	ran, err := primitives.RunCommand(ctx, primitives.Command{Arguments: []string{"dnf", "--quiet", "--disablerepo=*", "needs-restarting", option}, RunAs: &primitives.ExecOptions{RunAsUserAccount: primitives.UnprivilegedAccount, RunFromDirectory: "/", Env: map[string]string{"LC_ALL": "C"}}})
	var notFound *exec.Error
	switch {
	case errors.As(err, &notFound):
		o := Result("unknown", "MMWatchdog cannot find the program dnf, which says on this distribution whether the node needs a reboot")
		return ran, &o
	case err != nil:
		o := Result("unknown", "MMWatchdog cannot run dnf needs-restarting: "+err.Error())
		return ran, &o
	}
	return ran, nil
}

// rebootHintOfDNFFive asks dnf 5 in JSON. needs-restarting is a command of
// the package dnf5-plugins, which Fedora installs. Its exit code is 0 when
// the node needs no reboot and 1 when it does, as in dnf 4, and the JSON says
// the same in the field reboot_required, with the packages that are the
// reason. The check requires both to agree: an exit code and an answer that
// disagree, or an answer of another form, make the check unknown. dnf 5
// ignores the option --reboothint, so the check does not pass it. Like dnf
// 4's command, it only reads, so it runs as the unprivileged account and
// gets no repository.
func rebootHintOfDNFFive(ctx context.Context) Observation {
	ran, problem := runDNFNeedsRestarting(ctx, "--json")
	if problem != nil {
		return *problem
	}
	if ran.ExitCode == 2 && strings.Contains(ran.StandardError, `Unknown argument "needs-restarting"`) {
		return Result("unknown", "the dnf of this node has no command needs-restarting, which says whether the node needs a reboot: install the package of dnf's plugins, with dnf install dnf5-plugins")
	}
	var answer []struct {
		Type           *string  `json:"type"`
		RebootRequired *bool    `json:"reboot_required"`
		Packages       []string `json:"packages"`
	}
	if json.Unmarshal([]byte(ran.StandardOutput), &answer) != nil || len(answer) != 1 || answer[0].Type == nil || *answer[0].Type != "reboot" || answer[0].RebootRequired == nil ||
		(ran.ExitCode != 0 && ran.ExitCode != 1) || (ran.ExitCode == 1) != *answer[0].RebootRequired {
		return Result("unknown", primitives.DetailLine(fmt.Sprintf("dnf needs-restarting ended with exit code %d and an answer that MMWatchdog does not understand: %s", ran.ExitCode, primitives.Clip(strings.TrimSpace(ran.StandardOutput+" "+ran.StandardError), 200))))
	}
	if !*answer[0].RebootRequired {
		return Result("ok", nodeNeedsNoReboot)
	}
	o := Result("failed", "this node needs a reboot (dnf needs-restarting)")
	if packages := uniqueSorted(answer[0].Packages); len(packages) > 0 {
		o.Details = []string{primitives.DetailLine("Core libraries or services have been updated since boot-up: " + shownNames(packages))}
	}
	return o
}

func rebootRequiredOnDebianOrUbuntu() Observation {
	const healthy = nodeNeedsNoReboot
	// A kernel hook or a package that needs a reboot leaves this file,
	// and its name in the file beside it.
	if _, err := primitives.InformationAboutFile(primitives.NodeFile("/run/reboot-required")); errors.Is(err, fs.ErrNotExist) {
		if !slices.ContainsFunc(kernelHooksThatWriteRebootRequired, func(hook string) bool { return primitives.IsExecutableFile(primitives.NodeFile(hook)) }) {
			return Result("unknown", "no program on this node writes /run/reboot-required when a new kernel is installed, so MMWatchdog cannot know whether the node needs a reboot: install the package unattended-upgrades")
		}
		return Result("ok", healthy)
	} else if err != nil {
		return Result("unknown", "MMWatchdog cannot look at /run/reboot-required: "+err.Error())
	}
	o := Result("failed", "this node needs a reboot (/run/reboot-required exists)")
	if packages, err := primitives.ReadFile(primitives.NodeFile("/run/reboot-required.pkgs"), primitives.ProcFileLimit); err == nil {
		if names := uniqueSorted(strings.Fields(string(packages))); len(names) > 0 {
			o.Message = "this node needs a reboot for " + countOf(len(names), "package", "packages") + ": " + shownNames(names)
		}
	}
	return o
}

// uniqueSorted returns the names without a name twice, sorted.
func uniqueSorted(names []string) []string {
	seen, unique := map[string]bool{}, []string{}
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			unique = append(unique, name)
		}
	}
	slices.Sort(unique)
	return unique
}

// rebootAfterUpdatesDescription is what "mmwatchdog describe-module reboot_after_updates" shows, and what
// validation knows about the module's inputs.
var rebootAfterUpdatesDescription = ModuleDescription{
	Summary: "Fails while the node needs a reboot, for example after a new kernel was installed. The check reads what the node's own distribution says about it.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "fail_when", Type: "string", Required: true, AllowedValues: []any{"the_node_must_reboot_to_use_installed_updates"}, Description: "The check fails while the node must reboot to use installed updates; write the_node_must_reboot_to_use_installed_updates, the only value."},
	},
	Examples: []ModuleExample{{
		Description:   "Tell the team when this node needs a reboot.",
		Configuration: "reboot_after_updates:\n  fail_when: the_node_must_reboot_to_use_installed_updates\nalert_severity: info\n",
	}},
}
