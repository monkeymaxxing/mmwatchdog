package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"golang.org/x/sys/unix"

	modulepages "mmwatchdog/docs/reference/modules"
	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

// The two commands that show what the configuration does not: which kind of
// module a name stands for, and where its code is.

// moduleDirsFor returns the two module directories of a command: those of
// its options, and for an option that is not given the node's own from the
// node settings in configDir.
func moduleDirsFor(configDir string, stated checksandalerts.ModuleDirs) checksandalerts.ModuleDirs {
	dirs := config.NodeModuleDirs(configDir)
	if stated.OfficialExtra != "" {
		dirs.OfficialExtra = stated.OfficialExtra
	}
	if stated.External != "" {
		dirs.External = stated.External
	}
	return dirs
}

// kindOf writes the kind of a module in a sentence, such as "an official
// extra module (Starlark, shipped with MMWatchdog 1.0.0)".
func kindOf(info checksandalerts.ModuleInfo) string {
	switch info.Kind {
	case "core":
		return "a core module (built into MMWatchdog)"
	case "official_extra":
		return "an official extra module (Starlark, shipped with MMWatchdog " + info.ShippedWithMMWatchdogVersion + ")"
	case "project_starlark":
		return "a module of the project's own (Starlark)"
	case "project_executable":
		return "a module of the project's own (executable)"
	}
	return "a module that MMWatchdog cannot use"
}

// executableNote is what MMWatchdog says beside every module that is an executable,
// in two sentences.
var executableNote = [2]string{"MMWatchdog does not see what an executable does on this node.", "The local node must provide everything the executable needs."}

// moduleReadmeFile is the page of a module that is not core, in the
// module's directory. A deploy installs an official extra module's page on
// every node with the module's directory.
const moduleReadmeFile = "README.md"

// describeModule prints the whole page of one module, or with
// referenceOnly only its reference: what it is, its inputs, its
// measurements, and its examples. Without a name it lists every module
// that this node has, the core modules by topic.
func describeModule(configDir string, stated checksandalerts.ModuleDirs, name string, asJSON, referenceOnly bool) error {
	dirs := moduleDirsFor(configDir, stated)
	if name == "" {
		if referenceOnly {
			return fmt.Errorf("--reference-only needs the name of a module: mmwatchdog describe-module NAME --reference-only")
		}
		return listModules(dirs, asJSON)
	}
	module, e := config.DescribeModule(dirs, name)
	if e != nil {
		return e
	}
	page := ""
	if !referenceOnly {
		if page, e = modulePage(module); e != nil {
			return e
		}
	}
	if asJSON {
		return printJSON(struct {
			checksandalerts.ModuleInfo
			Page string `json:"page,omitempty"`
		}{module, page})
	}
	reference := moduleReference(module)
	switch {
	case referenceOnly:
		return printText(reference, false)
	case page == "":
		return printText(reference, true)
	case module.Kind == "core" || module.Kind == "official_extra":
		return printText(page, true)
	}
	// A module of the project's own: its reference, then its page.
	return printText(reference+"\n"+page, true)
}

// modulePage returns the page of a module: for a core module the page that
// is built into mmwatchdog, for every other module the README.md in its directory.
// It is "" for a module without a page.
func modulePage(module checksandalerts.ModuleInfo) (string, error) {
	if module.Kind == "core" {
		page, _ := modulepages.CoreModulePage(module.Name)
		return page, nil
	}
	readme, e := os.ReadFile(filepath.Join(module.Directory, moduleReadmeFile))
	if errors.Is(e, fs.ErrNotExist) {
		return "", nil
	}
	return string(readme), e
}

// listModules prints every module that this node has, one line each: the
// core modules by topic, then the official extra modules, then the
// project's own modules.
func listModules(dirs checksandalerts.ModuleDirs, asJSON bool) error {
	modules, e := config.ListModules(dirs)
	if e != nil {
		return e
	}
	if asJSON {
		return printJSON(struct {
			Modules []checksandalerts.ModuleInfo `json:"modules"`
		}{modules})
	}
	var text strings.Builder
	table := tabwriter.NewWriter(&text, 0, 0, 3, ' ', 0)
	line := func(module checksandalerts.ModuleInfo) {
		if module.Description == nil {
			fmt.Fprintf(table, "  %s\tMMWatchdog cannot use this module: %s\n", module.Name, module.Problem)
			return
		}
		fmt.Fprintf(table, "  %s\t%s\t%s\n", module.Name, usedAs(module.Description.UsedAs), module.Description.Summary)
	}
	_, _ = fmt.Fprintln(table, "Core modules (built into MMWatchdog), by topic") // Display failure changes no monitoring state or recorded operation result.
	for _, topic := range checksandalerts.CoreModuleTopics {
		fmt.Fprintf(table, "\n%s\n", topic)
		for _, module := range modules {
			if module.Kind == "core" && module.Topic == topic {
				line(module)
			}
		}
	}
	heading := ""
	for _, module := range modules {
		if module.Kind == "core" {
			continue
		}
		group := "The project's own modules"
		if module.Kind == "official_extra" {
			group = "Official extra modules (Starlark, shipped with MMWatchdog " + module.ShippedWithMMWatchdogVersion + ")"
		}
		if group != heading {
			fmt.Fprintf(table, "\n%s\n", group)
			heading = group
		}
		line(module)
	}
	table.Flush()
	text.WriteString("\nmmwatchdog describe-module NAME prints the whole page of one module: what it checks or where it delivers alerts, its inputs, measurements, and examples, how it works, its errors, and why it works this way. Add --reference-only for only the module's inputs, measurements, and examples.\n")
	return printText(text.String(), false)
}

// moduleReference is the reference of one module: its kind, what it is
// used as, its summary, its inputs, its measurements, and its examples.
func moduleReference(module checksandalerts.ModuleInfo) string {
	var text strings.Builder
	description := module.Description
	fmt.Fprintf(&text, "%s: %s, used as %s\n%s\n", module.Name, kindOf(module), usedAs(description.UsedAs), description.Summary)
	if module.Directory != "" {
		fmt.Fprintf(&text, "Files: %s/\n", strings.TrimSuffix(module.Directory, "/"))
	}
	if module.Kind == "project_executable" {
		fmt.Fprintln(&text, executableNote[0], executableNote[1])
	}
	if description.OneIncidentForEachFinding {
		fmt.Fprintln(&text, "One run of the check can report several findings, and each finding is its own incident.")
	}
	table := tabwriter.NewWriter(&text, 0, 0, 3, ' ', 0)
	inputLine := func(name string, input checksandalerts.ModuleInput) {
		kind := input.Type
		if len(input.AllowedValues) > 0 {
			values := []string{}
			for _, value := range input.AllowedValues {
				values = append(values, fmt.Sprint(value))
			}
			kind = strings.Join(values, " | ")
		}
		required := "optional"
		if input.Required {
			required = "required"
		}
		fmt.Fprintf(table, "  %s\t%s\t%s\t%s\n", name, kind, required, input.Description)
	}
	if description.Value != nil {
		_, _ = fmt.Fprintln(table, "\nValue") // Display failure changes no monitoring state or recorded operation result.
		inputLine("(the value of "+module.Name+")", *description.Value)
	}
	if len(description.Inputs) > 0 {
		_, _ = fmt.Fprintln(table, "\nInputs") // Display failure changes no monitoring state or recorded operation result.
		for _, input := range description.Inputs {
			inputLine(input.Name, input)
		}
	}
	if len(description.Measurements) > 0 {
		_, _ = fmt.Fprintln(table, "\nMeasurements") // Display failure changes no monitoring state or recorded operation result.
		for _, measurement := range description.Measurements {
			fmt.Fprintf(table, "  %s\t%s\t%s\n", measurement.Name, measurement.Unit, measurement.Description)
		}
	}
	table.Flush()
	for _, example := range description.Examples {
		fmt.Fprintf(&text, "\nExample: %s\n  %s\n", example.Description, strings.ReplaceAll(strings.TrimSuffix(example.Configuration, "\n"), "\n", "\n  "))
	}
	return text.String()
}

// printText writes the output of describe-module. A page goes through the
// user's pager when standard output is a terminal: $PAGER when it is set,
// otherwise less -FRX. When the pager cannot start, the page is printed.
func printText(text string, isPage bool) error {
	_, terminal := terminalSize(os.Stdout)
	if isPage && terminal {
		command := []string{"less", "-FRX"}
		if fromEnvironment := strings.Fields(os.Getenv("PAGER")); len(fromEnvironment) > 0 {
			command = fromEnvironment
		}
		pager := exec.Command(command[0], command[1:]...)
		pager.Stdin = strings.NewReader(text)
		pager.Stdout, pager.Stderr = os.Stdout, os.Stderr
		if pager.Start() == nil {
			// The pager's exit status says nothing about the page, such as
			// a person who quit before its end.
			_ = pager.Wait()
			return nil
		}
	}
	_, e := io.WriteString(os.Stdout, text)
	return e
}

// terminalSize returns the column count and whether the file is a terminal.
// Pipes and terminals that report no columns use the same 80-column default.
func terminalSize(file *os.File) (int, bool) {
	size, err := unix.IoctlGetWinsize(int(file.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 80, false
	}
	if size.Col == 0 {
		return 80, true
	}
	return int(size.Col), true
}

// usedAs writes a module's used_as for a person.
func usedAs(value string) string {
	if value == "alert_destination" {
		return "an alert destination"
	}
	return "a check"
}

// moduleUsage prints the modules that the node's configuration names: the
// kind of each, the place of its files, and every place that calls it.
func moduleUsage(configDir string, stated checksandalerts.ModuleDirs, name string, asJSON bool) error {
	c, e := config.LoadWithModuleDirs(configDir, stated)
	if e != nil {
		return e
	}
	uses := c.ModuleUsage()
	if name != "" {
		one := []config.ModuleUse{}
		for _, use := range uses {
			if use.Name == name {
				one = append(one, use)
			}
		}
		if len(one) == 0 {
			return fmt.Errorf("no check or alert destination in configuration directory %s uses module %s", configDir, name)
		}
		uses = one
	}
	fromTheHub := c.JoinFleetHub != nil
	if asJSON {
		return printJSON(struct {
			Modules                         []config.ModuleUse `json:"modules"`
			AlertDestinationsComeFromTheHub bool               `json:"alert_destinations_come_from_the_hub"`
		}{uses, fromTheHub})
	}
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	called := func(use config.ModuleUse) string {
		places := []string{}
		for _, at := range use.CalledAt {
			places = append(places, fmt.Sprintf("%s:%d", at.File, at.Line))
		}
		return strings.Join(places, ", ")
	}
	width := 0
	for _, use := range uses {
		width = max(width, len(use.Name))
	}
	heading := ""
	for _, use := range uses {
		group := map[string]string{"core": "Core modules (built into MMWatchdog)", "official_extra": "Official extra modules (Starlark, shipped with MMWatchdog " + use.ShippedWithMMWatchdogVersion + ")"}[use.Kind]
		if group == "" {
			group = "The project's own modules"
		}
		if group != heading {
			if heading != "" {
				_, _ = fmt.Fprintln(out) // Display failure changes no monitoring state or recorded operation result.
			}
			_, _ = fmt.Fprintln(out, group) // Display failure changes no monitoring state or recorded operation result.
			heading = group
		}
		indent := strings.Repeat(" ", width+5)
		switch use.Kind {
		case "core":
			fmt.Fprintf(out, "  %-*s   %s\n", width, use.Name, called(use))
		case "official_extra":
			fmt.Fprintf(out, "  %-*s   %s/\n%s%s\n", width, use.Name, use.Directory, indent, called(use))
		case "project_starlark":
			fmt.Fprintf(out, "  %-*s   %s/   Starlark\n%s%s\n", width, use.Name, use.Directory, indent, called(use))
		default:
			fmt.Fprintf(out, "  %-*s   %s/   executable\n%s%s\n%s%s\n%s%s\n", width, use.Name, use.Directory, indent, called(use), indent, executableNote[0], indent, executableNote[1])
		}
	}
	if len(uses) == 0 {
		_, _ = fmt.Fprintln(out, "The configuration names no module.") // Display failure changes no monitoring state or recorded operation result.
	}
	if fromTheHub {
		_, _ = fmt.Fprintln(out, "\nThis node has joined a hub, and its alert destinations come from the hub. Run mmwatchdog module-usage on the hub to see the modules of the destinations.") // Display failure changes no monitoring state or recorded operation result.
	}
	return nil
}
