package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"mmwatchdog/internal/store"
)

// ReadingNode holds the saved report used to identify checks and node groups.
// The command reads membership without starting a hub or loading its secrets.
type ReadingNode struct {
	Name   string
	Report *NodeStatusReport
	// OwnHistory is the hub's own credentialless entry. Its readings stay
	// in measurement_history.db rather than being copied to nodes' history.
	OwnHistory bool
}

// ReadReadingNodes selects a node or a node group from the saved fleet.
// A stopped hub has the same membership as its last committed report.
func ReadReadingNodes(path, node, nodeGroup string) ([]ReadingNode, error) {
	database, err := store.OpenReadOnlyDatabase(path)
	if err != nil {
		return nil, err
	}
	defer database.Close()
	rows, err := database.Query("SELECT name, data FROM nodes ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("cannot read saved fleet membership from %s: %w; check fleet.db on the fleet hub", path, err)
	}
	nodes := []ReadingNode{}
	for rows.Next() {
		var name string
		var data []byte
		if err = rows.Scan(&name, &data); err != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return nil, err
		}
		// Use the membership decoder's rule for the latest node settings.
		// Credential hashes are never part of a readings result.
		var saved member
		if err = json.Unmarshal(data, &saved); err != nil {
			_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
			return nil, fmt.Errorf("cannot read the saved report of node %q: %w; check fleet.db", name, err)
		}
		if nodeGroup != "" && slices.Contains(saved.fleetSettings().ThisNodeIsInNodeGroups, nodeGroup) || nodeGroup == "" && name == node {
			nodes = append(nodes, ReadingNode{Name: name, Report: saved.NodeStatusReport, OwnHistory: saved.Credential == ""})
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		if nodeGroup != "" {
			return nil, fmt.Errorf("node group %q has no nodes; use mmwatchdog fleet to see the node groups", nodeGroup)
		}
		return nil, fmt.Errorf("the fleet hub has no record of node %q; use mmwatchdog fleet to see the node names", node)
	}
	return nodes, nil
}
