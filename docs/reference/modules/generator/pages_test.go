package main

// These tests read the repository's Markdown and Go files, so they live here
// and not in the packages of internal/ or in cmd/mmwatchdog, whose test programs also run without
// the source tree. Each check is a function that a test runs on the
// repository and another test runs on the fixtures in testdata/, which
// prove that the check finds each fault. The documentation map
// (docs/changing-mmwatchdog/documentation-map.md, "What the tests check") lists what they
// enforce.

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

// fixtureRepository is a small tree in the repository's layout with one
// fault of each kind that the link check looks for.
const fixtureRepository = "testdata/repository"

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := findRepositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// --- Every core module has a page source with the sections of the map.

func TestEveryCoreModuleHasAPageSourceWithTheSectionsOfTheMap(t *testing.T) {
	root := repositoryRoot(t)
	for _, module := range config.CoreModules() {
		text, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(pageSourceFile(module))))
		if errors.Is(err, os.ErrNotExist) {
			t.Errorf("core module %s has no page source %s", module.Name, pageSourceFile(module))
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parsePage(pageSourceFile(module), module.Name, module.UsedAs, true, string(text)); err != nil {
			t.Error(err)
		}
	}
}

// The generator's list of sections is the documentation map's table "The
// sections of a module page": the same sections in the same order, each
// with the same use on the page of a check module and of a destination
// module, and the same sections generated on a core module's page.
func TestPageSectionsAreTheSectionsOfTheMap(t *testing.T) {
	text, err := os.ReadFile(filepath.Join(repositoryRoot(t), "docs/changing-mmwatchdog/documentation-map.md"))
	if err != nil {
		t.Fatal(err)
	}
	inMap, err := sectionsOfTheMapTable(string(text))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inMap, pageSections) {
		t.Fatalf("the documentation map's table lists the sections\n%+v\nthe generator has\n%+v", inMap, pageSections)
	}
}

// sectionsOfTheMapTable reads the rows of the table "The sections of a
// module page" below the title's row: | `## Heading` | Holds | Check module
// | Destination module |. A use that starts with "Required" is required,
// "Not used" is not used, and every other use is optional. A use that says
// "generated on a core page" marks a generated section.
func sectionsOfTheMapTable(text string) ([]pageSectionRule, error) {
	use := func(cell string) sectionUse {
		switch {
		case strings.HasPrefix(cell, "Required"):
			return sectionRequired
		case cell == "Not used":
			return sectionNotUsed
		}
		return sectionOptional
	}
	var rules []pageSectionRule
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "| `## ") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 4 {
			return nil, fmt.Errorf("a row of the table has %d columns, not 4: %s", len(cells), line)
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		rules = append(rules, pageSectionRule{
			Heading:             strings.TrimSuffix(strings.TrimPrefix(cells[0], "`## "), "`"),
			OnCheckModulePage:   use(cells[2]),
			OnDestinationPage:   use(cells[3]),
			GeneratedOnCorePage: strings.Contains(cells[2]+cells[3], "generated on a core page"),
		})
	}
	return rules, nil
}

func TestTheMapTableReaderReadsEachUse(t *testing.T) {
	rules, err := sectionsOfTheMapTable("| `# <module>` | Title | Required | Required |\n" +
		"| `## Inputs` | Inputs | Required; generated on a core page | Required; generated on a core page |\n" +
		"| `## Measurements` | Readings | When the module measures; generated on a core page | Not used |\n" +
		"| `## Test the destination` | A test | Not used | When the destination can be tested by hand |\n")
	want := []pageSectionRule{
		{"Inputs", sectionRequired, sectionRequired, true},
		{"Measurements", sectionOptional, sectionNotUsed, true},
		{"Test the destination", sectionNotUsed, sectionOptional, false},
	}
	if err != nil || !reflect.DeepEqual(rules, want) {
		t.Fatalf("%+v %v", rules, err)
	}
}

func TestPageSourceCheckFindsEachFault(t *testing.T) {
	for _, tc := range []struct{ file, usedAs, want string }{
		{"valid_check_page.md", usedAsCheck, ""},
		{"valid_destination_page.md", usedAsAlertDestination, ""},
		{"page_with_wrong_title.md", usedAsCheck, "the first line must be the title # `page_with_wrong_title`"},
		{"page_with_unknown_section.md", usedAsCheck, `"## Notes" is not a section of a module page`},
		{"check_page_with_destination_section.md", usedAsCheck, `"## What it sends" is not a section of a check module's page`},
		{"valid_check_page.md", usedAsAlertDestination, `"## When the check fails, and when it cannot check" is not a section of a destination module's page`},
		{"page_with_sections_reversed.md", usedAsCheck, `the section "## When the check fails, and when it cannot check" is out of order`},
		{"page_without_tested_on.md", usedAsCheck, `the required section "## Tested on" is missing`},
		{"valid_check_page.md", usedAsAlertDestination, `the required section "## What it sends" is missing`},
		{"page_with_generated_section.md", usedAsCheck, `the section "## Inputs" is generated from the module's description`},
		{"page_with_second_title.md", usedAsCheck, "a page has one title"},
	} {
		text, err := os.ReadFile(filepath.Join("testdata/page_sources", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		module := strings.TrimSuffix(tc.file, ".md")
		_, err = parsePage(tc.file, module, tc.usedAs, true, string(text))
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s as %s: got %v, want %q", tc.file, tc.usedAs, err, tc.want)
		}
	}
}

// coreModule returns the core module with that name.
func coreModule(t *testing.T, name string) config.CoreModule {
	t.Helper()
	for _, module := range config.CoreModules() {
		if module.Name == name {
			return module
		}
	}
	t.Fatalf("no core module is named %s", name)
	return config.CoreModule{}
}

// The generated page puts the source's sections around the generated ones in
// the map's order, and rewrites the source's links for docs/reference/modules/.
func TestGeneratedPageJoinsSourceAndDescription(t *testing.T) {
	text, err := os.ReadFile("testdata/page_sources/valid_check_page.md")
	if err != nil {
		t.Fatal(err)
	}
	source, err := parsePage("valid_check_page.md", "valid_check_page", usedAsCheck, true, string(text))
	if err != nil {
		t.Fatal(err)
	}
	page, err := renderPage(coreModule(t, "node_filesystem"), source)
	if err != nil {
		t.Fatal(err)
	}
	var headings []string
	for _, line := range linesOutsideCodeFences(strings.Split(page, "\n")) {
		if line.outsideCodeFence && strings.HasPrefix(line.text, "## ") {
			headings = append(headings, line.text)
		}
	}
	want := []string{"## Inputs", "## Measurements", "## When the check fails, and when it cannot check", "## Examples", "## Tested on", "## Why it works this way"}
	if !slices.Equal(headings, want) {
		t.Fatalf("headings %q, want %q", headings, want)
	}
	for _, line := range []string{
		"<!-- Generated by go generate ./docs/reference/modules from internal/checksandalerts/check_node_filesystem.md and the module's description in internal/checksandalerts/check_node_filesystem.go. Edit those files, then run go generate ./docs/reference/modules; never edit this file. -->\n# `valid_check_page`\n",
		"[`tcp`](tcp.md)",
		"[what an email destination sends](email.md#what-it-sends)",
		"[the failing time](../../usage.md#the-failing-time)",
		"[the code](../../../internal/checksandalerts/check_http.go)",
		"`[not a link](../../docs/usage.md)`",
		"[inside a code fence](check_tcp.md)",
		"\n- the value of `node_filesystem` (string; required): The absolute path",
		"\n- `space_used_percent` (percent): ",
		"A check without a limit can still fail, as [When the check fails, and when it cannot check](#when-the-check-fails-and-when-it-cannot-check) says",
		"[checks that measure a number](../configuration-files.md#checks-that-measure)",
	} {
		if !strings.Contains(page, line) {
			t.Errorf("the generated page lacks %q:\n%s", line, page)
		}
	}
	text, err = os.ReadFile("testdata/page_sources/valid_destination_page.md")
	if err != nil {
		t.Fatal(err)
	}
	source, err = parsePage("valid_destination_page.md", "valid_destination_page", usedAsAlertDestination, true, string(text))
	if err != nil {
		t.Fatal(err)
	}
	page, err = renderPage(coreModule(t, "webhook"), source)
	if err != nil {
		t.Fatal(err)
	}
	headings = nil
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, line)
		}
	}
	want = []string{"## Set up", "## Inputs", "## What it sends", "## When a delivery fails", "## Examples", "## Test the destination", "## Tested on", "## Why it works this way"}
	if !slices.Equal(headings, want) {
		t.Fatalf("headings %q, want %q", headings, want)
	}
}

// The generated "## Inputs" says how a check or a destination writes the
// module: inputs, or one value in place of them.
func TestGeneratedInputsSayHowTheModuleIsWritten(t *testing.T) {
	for name, lines := range map[string][]string{
		"http":                 {"- `url` (string; required): ", "- `method` (`GET` or `HEAD`; optional): "},
		"tcp":                  {"A check writes the module's inputs as a mapping below the module's name. In place of the mapping, it can write one value after the name:\n\n- the value of `tcp` (string): ", "\nThe inputs of the mapping:\n\n- `address` ("},
		"ssh":                  {"The module takes one value, which a check writes after the module's name, and no other input.\n\n- the value of `ssh` (string; required): "},
		"reboot_after_updates": {"- `fail_when` (`the_node_must_reboot_to_use_installed_updates`; required): "},
	} {
		got := renderInputs(coreModule(t, name))
		for _, line := range lines {
			if !strings.Contains(got, line) {
				t.Errorf("%s: the inputs lack %q:\n%s", name, line, got)
			}
		}
	}
	if got := renderInputs(config.CoreModule{Name: "nothing", UsedAs: usedAsAlertDestination, Description: &checksandalerts.ModuleDescription{}}); got != "The module takes no inputs.\n" {
		t.Errorf("a module without inputs: %q", got)
	}
}

// The generated "## Measurements" says whether a check can set a limit, as
// the module's registry line says.
func TestGeneratedMeasurementsSayWhetherACheckSetsALimit(t *testing.T) {
	for name, want := range map[string]string{
		"node_resource":   "A check that only records its readings states `only_record_readings: true`",
		"node_filesystem": "A check without a limit can still fail",
		"ping":            "The module fails a check by itself, and it also reports these measurements.",
		"tls":             "A check takes no limit on these measurements: the module's own inputs say when the check fails.",
	} {
		got := renderMeasurements(coreModule(t, name))
		if !strings.Contains(got, want) || strings.Contains(got, "("+limitsSection+")") == (name == "tls") {
			t.Errorf("%s:\n%s", name, got)
		}
	}
}

// --- Every official extra module's README.md has the sections of the map.

func TestOfficialExtraModulePagesHaveTheSectionsOfTheMap(t *testing.T) {
	root := repositoryRoot(t)
	extras, err := officialExtraModules(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(extras) == 0 {
		t.Fatal("the repository has no official extra module, so this test checks nothing")
	}
	for _, module := range extras {
		file := officialExtraModuleDirectory + "/" + module.Name + "/" + officialExtraModulePage
		text, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			t.Error(err)
			continue
		}
		var inputs []string
		for _, input := range module.Description.Inputs {
			inputs = append(inputs, input.Name)
		}
		if err := checkOfficialExtraModulePage(file, module.Name, module.Description.UsedAs, inputs, string(text)); err != nil {
			t.Error(err)
		}
	}
}

func TestOfficialExtraModulePageCheckFindsEachFault(t *testing.T) {
	inputs := []string{"first_input", "second_input"}
	for file, want := range map[string]string{
		"valid_official_page.md":             "",
		"official_page_without_inputs.md":    `the required section "## Inputs" is missing`,
		"official_page_with_other_inputs.md": "the table of \"## Inputs\" names the inputs first_input, third_input; module_description.yml has the inputs first_input, second_input",
	} {
		text, err := os.ReadFile(filepath.Join("testdata/official_pages", file))
		if err != nil {
			t.Fatal(err)
		}
		err = checkOfficialExtraModulePage(file, strings.TrimSuffix(file, ".md"), usedAsCheck, inputs, string(text))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: got %v, want %q", file, err, want)
		}
	}
	// The same page as a destination's has the sections of a check.
	text, err := os.ReadFile("testdata/official_pages/valid_official_page.md")
	if err != nil {
		t.Fatal(err)
	}
	err = checkOfficialExtraModulePage("valid_official_page.md", "valid_official_page", usedAsAlertDestination, inputs, string(text))
	if err == nil || !strings.Contains(err.Error(), "is not a section of a destination module's page") {
		t.Fatal(err)
	}
}

// checkOfficialExtraModulePage checks the README.md of an official extra
// module: the sections of the map for its kind, all required sections among
// them "## Inputs", and an "## Inputs" table whose first column names
// exactly the inputs of its module_description.yml.
func checkOfficialExtraModulePage(file, moduleName, usedAs string, inputs []string, text string) error {
	source, err := parsePage(file, moduleName, usedAs, false, text)
	if err != nil {
		return err
	}
	section, _ := source.section("Inputs")
	named := inputsTableNames(section.Text)
	if !slices.Equal(slices.Sorted(slices.Values(named)), slices.Sorted(slices.Values(inputs))) {
		return fmt.Errorf("%s: the table of \"## Inputs\" names the inputs %s; module_description.yml has the inputs %s", file, joinedOrNone(named), joinedOrNone(inputs))
	}
	return nil
}

func joinedOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// inputsTableNames returns the first column of the first table of a
// section, below its header and its separator row, without backticks.
func inputsTableNames(text string) []string {
	var names []string
	row := 0
	for _, line := range linesOutsideCodeFences(strings.Split(text, "\n")) {
		trimmed := strings.TrimSpace(line.text)
		if !line.outsideCodeFence || !strings.HasPrefix(trimmed, "|") {
			if row > 0 {
				break
			}
			continue
		}
		row++
		if row <= 2 {
			continue
		}
		first, _, _ := strings.Cut(strings.TrimPrefix(trimmed, "|"), "|")
		names = append(names, strings.Trim(strings.TrimSpace(first), "`"))
	}
	return names
}

// --- The generated files equal what the generator writes now.

func TestGeneratedFilesAreCurrent(t *testing.T) {
	root := repositoryRoot(t)
	generated, _, err := generatePages(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range compareGeneratedFiles(filepath.Join(root, filepath.FromSlash(generatedPageDirectory)), generated) {
		t.Error(problem)
	}
}

func TestGeneratedFileComparisonFindsEachFault(t *testing.T) {
	directory := t.TempDir()
	for name, text := range map[string]string{
		"current.md":  generatedFileMarker + " -->\ncurrent\n",
		"changed.md":  generatedFileMarker + " -->\nedited by hand\n",
		"removed.md":  generatedFileMarker + " -->\na module that is gone\n",
		"handmade.md": "not generated\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	problems := compareGeneratedFiles(directory, map[string][]byte{
		"current.md": []byte(generatedFileMarker + " -->\ncurrent\n"),
		"changed.md": []byte(generatedFileMarker + " -->\nnew\n"),
		"missing.md": []byte(generatedFileMarker + " -->\nnew page\n"),
	})
	joined := strings.Join(problems, "\n")
	if len(problems) != 3 || !strings.Contains(joined, "changed.md differs") || !strings.Contains(joined, "missing.md is missing") || !strings.Contains(joined, "removed.md is a generated file") {
		t.Fatalf("problems: %q", problems)
	}
}

// compareGeneratedFiles returns a problem for each generated file in
// directory that differs from the generator's output, is missing, or is no
// longer generated.
func compareGeneratedFiles(directory string, generated map[string][]byte) []string {
	const remedy = "; run go generate ./docs/reference/modules"
	var problems []string
	names := make([]string, 0, len(generated))
	for name := range generated {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		file := filepath.Join(directory, name)
		text, err := os.ReadFile(file)
		switch {
		case errors.Is(err, os.ErrNotExist):
			problems = append(problems, file+" is missing"+remedy)
		case err != nil:
			problems = append(problems, err.Error())
		case !bytes.Equal(text, generated[name]):
			problems = append(problems, file+" differs from what the generator writes now"+remedy)
		}
	}
	existing, _ := filepath.Glob(filepath.Join(directory, "*.md"))
	for _, file := range existing {
		if _, ok := generated[filepath.Base(file)]; ok {
			continue
		}
		if text, err := os.ReadFile(file); err == nil && bytes.HasPrefix(text, []byte(generatedFileMarker)) {
			problems = append(problems, file+" is a generated file that the generator no longer writes"+remedy)
		}
	}
	return problems
}

// --- Every relative link of the repository's Markdown files resolves.

// repositoryMarkdownFiles lists the Markdown files that Git tracks or would
// track, relative to the root, without the fixtures in testdata/.
func repositoryMarkdownFiles(t *testing.T, root string) []string {
	t.Helper()
	command := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "--", "*.md")
	command.Dir = root
	listed, err := command.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, file := range strings.Split(strings.TrimSpace(string(listed)), "\n") {
		if file != "" && !strings.HasPrefix(file, "_") && !slices.Contains(strings.Split(file, "/"), "testdata") {
			if _, err := os.Stat(filepath.Join(root, file)); err == nil {
				files = append(files, file)
			}
		}
	}
	return files
}

func TestRelativeLinksResolve(t *testing.T) {
	root := repositoryRoot(t)
	for _, problem := range checkRelativeLinks(root, repositoryMarkdownFiles(t, root)) {
		t.Error(problem)
	}
}

func TestLinkCheckFindsEachFault(t *testing.T) {
	problems := checkRelativeLinks(fixtureRepository, []string{"docs/links.md"})
	want := []string{
		"docs/links.md:5: missing.md: no such file",
		"docs/links.md:6: usage.md#no-such-heading: usage.md has no heading with the anchor no-such-heading",
		"docs/links.md:7: #not-here: docs/links.md has no heading with the anchor not-here",
		"docs/links.md:8: usage.md#repeated-2: usage.md has no heading with the anchor repeated-2",
	}
	if !slices.Equal(problems, want) {
		t.Fatalf("problems:\n%s\nwant:\n%s", strings.Join(problems, "\n"), strings.Join(want, "\n"))
	}
}

func TestGitHubAnchors(t *testing.T) {
	for heading, want := range map[string]string{
		"The modules of a node": "the-modules-of-a-node",
		"`http`":                "http",
		"When the check fails, and when it cannot check": "when-the-check-fails-and-when-it-cannot-check",
		"The node's resources, storage, and hardware":    "the-nodes-resources-storage-and-hardware",
		"A literal `{{` in a value":                      "a-literal--in-a-value",
		"Coming from [Nagios](nagios.md) or Monit":       "coming-from-nagios-or-monit",
		"PostgreSQL, MySQL, and MariaDB checks":          "postgresql-mysql-and-mariadb-checks",
	} {
		if got := githubAnchor(heading); got != want {
			t.Errorf("%q: got %q, want %q", heading, got, want)
		}
	}
}

// checkRelativeLinks returns a problem for each relative link in files, paths
// relative to root, that leads to no file, or to no heading of the file.
func checkRelativeLinks(root string, files []string) []string {
	anchors := map[string]map[string]bool{}
	var problems []string
	for _, file := range files {
		text, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for number, line := range linesOutsideCodeFences(strings.Split(string(text), "\n")) {
			if !line.outsideCodeFence {
				continue
			}
			for _, target := range linkTargets(line.text) {
				if urlScheme.MatchString(target) || strings.HasPrefix(target, "/") {
					continue
				}
				linkPath, anchor, _ := strings.Cut(target, "#")
				unescaped, err := url.PathUnescape(linkPath)
				if err != nil {
					unescaped = linkPath
				}
				resolved := file
				if unescaped != "" {
					resolved = path.Clean(path.Join(path.Dir(file), unescaped))
				}
				if _, err := os.Stat(filepath.Join(root, resolved)); err != nil {
					problems = append(problems, fmt.Sprintf("%s:%d: %s: no such file", file, number+1, target))
					continue
				}
				if anchor == "" || path.Ext(resolved) != ".md" {
					continue
				}
				if anchors[resolved] == nil {
					anchors[resolved] = headingAnchors(filepath.Join(root, resolved))
				}
				if !anchors[resolved][anchor] {
					name := linkPath
					if name == "" {
						name = file
					}
					problems = append(problems, fmt.Sprintf("%s:%d: %s: %s has no heading with the anchor %s", file, number+1, target, name, anchor))
				}
			}
		}
	}
	return problems
}

// linkTargets returns the targets of the links in a Markdown text, outside
// code fences and code spans.
func linkTargets(text string) []string {
	var targets []string
	replaceLinkTargets(text, func(target string) string {
		targets = append(targets, target)
		return target
	})
	return targets
}

// headingAnchors returns the anchors of a Markdown file's headings.
func headingAnchors(file string) map[string]bool {
	text, err := os.ReadFile(file)
	anchors := map[string]bool{}
	if err != nil {
		return anchors
	}
	for _, heading := range headingsOf(string(text)) {
		anchors[heading.anchor] = true
	}
	return anchors
}

// markdownHeading is one heading of a file: its level, its anchor, and the
// index of its line.
type markdownHeading struct {
	level  int
	anchor string
	line   int
}

var atxHeading = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)

// headingsOf returns the headings of a Markdown text outside code fences,
// with GitHub's anchors: the second heading with the same anchor gets "-1",
// the third "-2".
func headingsOf(text string) []markdownHeading {
	var headings []markdownHeading
	used := map[string]int{}
	for number, line := range linesOutsideCodeFences(strings.Split(text, "\n")) {
		if !line.outsideCodeFence {
			continue
		}
		parts := atxHeading.FindStringSubmatch(line.text)
		if parts == nil {
			continue
		}
		base := githubAnchor(parts[2])
		anchor := base
		for {
			if _, taken := used[anchor]; !taken {
				break
			}
			used[base]++
			anchor = fmt.Sprintf("%s-%d", base, used[base])
		}
		used[anchor] = 0
		headings = append(headings, markdownHeading{len(parts[1]), anchor, number})
	}
	return headings
}

var (
	inlineLink = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	htmlTag    = regexp.MustCompile(`<[^>]+>`)
)

// githubAnchor is the anchor GitHub gives a heading's text: the text as
// rendered, in lower case, without punctuation, with each space a hyphen.
func githubAnchor(heading string) string {
	text := inlineLink.ReplaceAllString(heading, "$1")
	text = htmlTag.ReplaceAllString(text, "")
	var anchor strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r == ' ':
			anchor.WriteRune('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.M, r):
			anchor.WriteRune(r)
		}
	}
	return anchor.String()
}

// --- The pages come into the mmwatchdog program only through its command line.

// Only cmd/mmwatchdog imports the package of the pages, so the packages of
// internal/, the code of the node monitoring process and of every check,
// hold no page text, and
// "mmwatchdog describe-module" is the one way the pages reach a node.
func TestOnlyTheMMWatchdogCommandLineImportsThePages(t *testing.T) {
	command := exec.Command("go", "list", "-f", `{{.ImportPath}}{{range .Imports}} {{.}}{{end}}`, "./...")
	command.Dir = repositoryRoot(t)
	listed, err := command.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	const pagePackage, commandLine = "mmwatchdog/docs/reference/modules", "mmwatchdog/cmd/mmwatchdog"
	importers := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(listed)), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && slices.Contains(fields[1:], pagePackage) {
			importers = append(importers, fields[0])
		}
	}
	if !slices.Equal(importers, []string{commandLine}) {
		t.Fatalf("the packages that import %s are %q; only %s may", pagePackage, importers, commandLine)
	}
}

// A description's plain text keeps its placeholders, asterisks, and
// backslashes visible on the web and in a terminal by showing them as code.
func TestDescriptionTextShowsSpecialWordsAsCode(t *testing.T) {
	for text, want := range map[string]string{
		"writes /etc/mmwatchdog/check_groups/<name>.yml, root-owned": "writes `/etc/mmwatchdog/check_groups/<name>.yml`, root-owned",
		`such as "15 3 * * *", or @daily`:                            "such as `\"15 3 * * *\"`, or @daily",
		"matches *.log (or app.*).":                                  "matches `*.log` (or `app.*`).",
		"already `<name>` as code":                                   "already `<name>` as code",
		"plain text stays plain":                                     "plain text stays plain",
	} {
		if got := escapeMarkdown(text); got != want {
			t.Errorf("escapeMarkdown(%q) = %q, want %q", text, got, want)
		}
	}
}

// --- Every name that users read follows the env, config, and dir table.

// docs/changing-mmwatchdog/design/README.md, "env, config, and dir have one spelling", gives
// three words one spelling in names: env for environment variables, config
// for configuration, and dir in a label without a verb, with directory in the
// plural and as a name's first word. Whether a label has a verb stays a call
// for review.

// namingTableFault says how a name breaks the naming table, or is empty.
func namingTableFault(name string) string {
	words := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '_' || r == '.' || r == '-' })
	for i, word := range words {
		switch {
		case word == "environment" || word == "environments":
			return "write env for environment variables"
		case word == "configuration" || word == "configurations":
			return "write config for configuration"
		case word == "dirs":
			return "write directories in the plural"
		case word == "dir" && i == 0 && len(words) > 1:
			return "write directory as the first word"
		}
	}
	return ""
}

// namesUsersRead lists the YAML and JSON names of the Go code, and the
// inputs and measurements of the core modules and of the modules with a
// module description file, each with the place it comes from.
func namesUsersRead(t *testing.T, root string) map[string]string {
	t.Helper()
	names := map[string]string{}
	listed, err := exec.Command("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "--", "cmd/*.go", "internal/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := token.NewFileSet()
	for _, file := range strings.Fields(string(listed)) {
		if strings.HasSuffix(file, "_test.go") || slices.Contains(strings.Split(file, "/"), "testdata") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, file)); errors.Is(err, os.ErrNotExist) {
			continue
		}
		parsed, err := parser.ParseFile(files, filepath.Join(root, file), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			field, ok := n.(*ast.Field)
			if !ok || field.Tag == nil {
				return true
			}
			tag, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"yaml", "json"} {
				if name, _, _ := strings.Cut(reflect.StructTag(tag).Get(key), ","); name != "" && name != "-" {
					names[name] = fmt.Sprintf("%s:%d", file, files.Position(field.Pos()).Line)
				}
			}
			return true
		})
	}
	describe := func(description *checksandalerts.ModuleDescription, place string) {
		for _, input := range description.Inputs {
			names[input.Name] = place
		}
		for _, measurement := range description.Measurements {
			names[measurement.Name] = place
		}
	}
	for _, module := range config.CoreModules() {
		describe(module.Description, "the description of the core module "+module.Name)
	}
	descriptions, err := filepath.Glob(filepath.Join(root, "official_extra_modules", "*", checksandalerts.ModuleDescriptionFile))
	if err != nil {
		t.Fatal(err)
	}
	examples, err := filepath.Glob(filepath.Join(root, "examples", "modules", "*", checksandalerts.ModuleDescriptionFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range append(descriptions, examples...) {
		directory := filepath.Dir(path)
		description, err := checksandalerts.ReadModuleDescription(directory, filepath.Base(directory))
		if err != nil {
			t.Fatal(err)
		}
		place, _ := filepath.Rel(root, path)
		describe(description, place)
	}
	return names
}

func TestNamesFollowTheNamingTable(t *testing.T) {
	names := namesUsersRead(t, repositoryRoot(t))
	for _, required := range []string{"mmhotline_url", "time_limit_per_delivery_attempt", "smtp_connection_security", "webhook_url_file"} {
		if _, found := names[required]; !found {
			t.Fatalf("did not collect the user-facing name %s", required)
		}
	}
	for name, place := range names {
		if fault := namingTableFault(name); fault != "" {
			t.Errorf("%s: %s: %s (docs/changing-mmwatchdog/design/README.md, section %q)", place, name, fault, "env, config, and dir have one spelling")
		}
	}
}

func TestNamingTableCheckFindsEachFault(t *testing.T) {
	for name, want := range map[string]string{
		"secrets_from_environment":  "write env for environment variables",
		"configuration_fingerprint": "write config for configuration",
		"module_dirs":               "write directories in the plural",
		"dir_mode":                  "write directory as the first word",
		"secrets_from_env":          "",
		"config_fingerprint":        "",
		"state_dir":                 "",
		"run_from_directory":        "",
		"module_directories":        "",
		"directory":                 "",
		"dir":                       "",
	} {
		if got := namingTableFault(name); got != want {
			t.Errorf("namingTableFault(%q) = %q, want %q", name, got, want)
		}
	}
}
