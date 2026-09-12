package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// GetRunAgentSelection reads the selection pinned when a run started.
// NULL and empty payloads identify legacy runs. Config owns the JSON schema.
func (d *DB) GetRunAgentSelection(id string) (string, error) {
	var payload sql.NullString
	err := d.sql.QueryRow(`SELECT agent_selection_json FROM runs WHERE id = ?`, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get run agent selection: %w", err)
	}
	return payload.String, nil
}

// SetRunAgentSelection binds the selection before the executor starts. A
// repeated write may confirm the same payload, but cannot retarget the run.
func (d *DB) SetRunAgentSelection(id, payload string) error {
	result, err := d.sql.Exec(`UPDATE runs SET agent_selection_json = ?, updated_at = ?
		WHERE id = ? AND (agent_selection_json IS NULL OR agent_selection_json = '' OR agent_selection_json = ?)`,
		payload, now(), id, payload)
	if err != nil {
		return fmt.Errorf("set run agent selection: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check run agent selection: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("run %q does not exist or already has a different agent selection", id)
	}
	return nil
}
