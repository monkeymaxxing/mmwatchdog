package fleet_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"mmwatchdog/internal/config"
	"mmwatchdog/internal/fleet"
)

func TestReadingNodesUseLatestSavedMembership(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	hub, err := fleet.OpenHub(directory, "hub.example.com:8445", "hub", time.Second, config.DefaultHubLimits, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := json.Marshal(map[string]any{
		"credential":                  "node-1-credential",
		"this_node_is_in_node_groups": []string{"old_apps"},
		"report":                      map[string]any{"credential": "node-2-credential", "this_node_is_in_node_groups": []string{"apps"}},
	})
	if _, err = hub.DB.Exec("INSERT INTO nodes(name,data) VALUES(?,?)", "node-1", saved); err != nil {
		t.Fatal(err)
	}
	saved, _ = json.Marshal(map[string]any{"credential": "node-2-credential", "this_node_is_in_node_groups": []string{"apps"}})
	if _, err = hub.DB.Exec("INSERT INTO nodes(name,data) VALUES(?,?)", "node-2", saved); err != nil {
		t.Fatal(err)
	}
	hub.Close()
	nodes, err := fleet.ReadReadingNodes(filepath.Join(directory, "fleet.db"), "", "apps")
	if err != nil || len(nodes) != 2 || nodes[0].Name != "node-1" || nodes[0].Report == nil || nodes[1].Name != "node-2" {
		t.Fatal(nodes, err)
	}
	if _, err = fleet.ReadReadingNodes(filepath.Join(directory, "fleet.db"), "", "old_apps"); err == nil {
		t.Fatal("used stale membership from the join")
	}
	nodes, err = fleet.ReadReadingNodes(filepath.Join(directory, "fleet.db"), "node-2", "")
	if err != nil || len(nodes) != 1 || nodes[0].Name != "node-2" {
		t.Fatal(nodes, err)
	}
}
