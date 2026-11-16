package checksandalerts

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

type SourceLocation struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

func (s SourceLocation) String() string { return fmt.Sprintf("%s:%d:%d", s.File, s.Line, s.Column) }

type FieldError struct{ Path, Message string }

func (e *FieldError) Error() string { return e.Path + ": " + e.Message }

func RecordSources(out map[string]SourceLocation, file, path string, n *yaml.Node) {
	out[path] = SourceLocation{file, n.Line, n.Column}
	// Only mappings and sequences have child paths; other nodes already have their own location recorded above.
	//exhaustive:ignore
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			child := key.Value
			if path != "" {
				child = path + "." + child
			}
			RecordSources(out, file, child, n.Content[i+1])
			out[child] = SourceLocation{file, key.Line, key.Column}
		}
	case yaml.SequenceNode:
		for i, child := range n.Content {
			RecordSources(out, file, fmt.Sprintf("%s.%d", path, i), child)
		}
	}
}
