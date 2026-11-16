// Package modulepages holds the generated pages of the core modules, so that
// mmwatchdog describe-module NAME prints the same page that a user reads in
// docs/reference/modules/. Only the mmwatchdog program's command line (cmd/mmwatchdog) imports this
// package: the person who needs a page at 3 AM is logged in on the node
// where a check fails, and MMWatchdog has no other program that could carry the
// pages.
//
// The generator in ./generator writes the pages from each module's page
// source internal/checksandalerts/check_<module>.md or internal/checksandalerts/destination_<module>.md
// and its description in the code file beside it. The documentation map
// (docs/changing-mmwatchdog/documentation-map.md, "Module pages") owns the rules.
package modulepages

import (
	"embed"
	"strings"
)

//go:generate go run ./generator

// generatedPages are the generated Markdown files of this directory: one
// page for each core module that has a page source, and the index README.md.
//
//go:embed *.md
var generatedPages embed.FS

// CoreModulePage returns the generated page of a core module without its
// first line, the comment that says the file is generated, and whether the
// module has a page.
func CoreModulePage(moduleName string) (string, bool) {
	if moduleName == "" || moduleName == "README" || strings.ContainsAny(moduleName, "/.") {
		return "", false
	}
	page, err := generatedPages.ReadFile(moduleName + ".md")
	if err != nil {
		return "", false
	}
	text := string(page)
	if strings.HasPrefix(text, "<!--") {
		_, text, _ = strings.Cut(text, "\n")
	}
	return text, true
}
