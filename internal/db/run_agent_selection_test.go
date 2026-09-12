package db

import (
	"path/filepath"
	"testing"
)

func TestRunAgentSelectionSurvivesMigrationAndReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "selection.sqlite")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	repo, err := d.InsertRepo("/fixture/project", "git@example.invalid:project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an existing database with a run created before the new column.
	if _, err := d.sql.Exec(`ALTER TABLE runs DROP COLUMN agent_selection_json`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.GetRunAgentSelection(run.ID); err != nil || got != "" {
		t.Fatalf("legacy selection = %q, %v", got, err)
	}
	payload := `{"version":1,"agent":"codex","agents":["codex"],"agent_config":{"codex":{"model":"gpt-5.6-sol","effort":"medium"}}}`
	if err := d.SetRunAgentSelection(run.ID, payload); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunAgentSelection(run.ID, payload); err != nil {
		t.Fatalf("idempotent write: %v", err)
	}
	if err := d.SetRunAgentSelection(run.ID, `{"different":true}`); err == nil {
		t.Fatal("changed selection was accepted")
	}
	if err := d.SetRunAgentSelection("missing", payload); err == nil {
		t.Fatal("missing run was accepted")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.GetRunAgentSelection(run.ID); err != nil || got != payload {
		t.Fatalf("reopened selection = %q, %v; want %q", got, err, payload)
	}
}
