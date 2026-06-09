package stats

import (
	"encoding/json"
	"fmt"
	"time"
)

// Episodic is a rule-based summary of one session's activity, generated when the
// session goes idle and surfaced in the next session_start for the workspace.
type Episodic struct {
	Workspace    string
	SessionID    string
	SessionName  string
	GeneratedAt  time.Time
	Summary      string // redacted, bounded
	TouchedFiles []string
	ReadCount    int
	WriteCount   int
}

// recordEpisodic inserts one episodic summary. Mutex-guarded like the other
// writes; called only from the writer goroutine via Writer.RecordEpisodic.
func (d *DB) recordEpisodic(e Episodic) error {
	if d == nil || e.Workspace == "" {
		return nil
	}
	touched, _ := json.Marshal(e.TouchedFiles)
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(`
		INSERT INTO episodic_memories
		  (workspace, session_id, session_name, generated_at, summary, touched_files, read_count, write_count)
		VALUES (?,?,?,?,?,?,?,?)`,
		e.Workspace, e.SessionID, e.SessionName, e.GeneratedAt.UnixMilli(),
		capString(e.Summary), string(touched), e.ReadCount, e.WriteCount)
	if err != nil {
		return fmt.Errorf("stats: insert episodic: %w", err)
	}
	return nil
}

// LatestEpisodic returns the most recent episodic summary for workspace, or
// ok=false when none exists. Read path — safe on a read-only handle.
func (d *DB) LatestEpisodic(workspace string) (Episodic, bool, error) {
	if d == nil || workspace == "" {
		return Episodic{}, false, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var (
		e       Episodic
		genMS   int64
		touched string
	)
	row := d.db.QueryRow(`
		SELECT workspace, session_id, session_name, generated_at, summary, touched_files, read_count, write_count
		FROM episodic_memories
		WHERE workspace = ?
		ORDER BY generated_at DESC
		LIMIT 1`, workspace)
	err := row.Scan(&e.Workspace, &e.SessionID, &e.SessionName, &genMS, &e.Summary,
		&touched, &e.ReadCount, &e.WriteCount)
	if err != nil {
		return Episodic{}, false, nil // no rows or read error → treat as absent
	}
	e.GeneratedAt = time.UnixMilli(genMS)
	_ = json.Unmarshal([]byte(touched), &e.TouchedFiles)
	return e, true, nil
}

// ToolCallsForSession returns the calls a session made in workspace since the
// given time, oldest first. Used by the episodic generator to derive a summary.
// Only the fields the generator needs are populated.
func (d *DB) ToolCallsForSession(workspace, sessionID string, since time.Time) ([]Call, error) {
	if d == nil || workspace == "" || sessionID == "" {
		return nil, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query(`
		SELECT tool, called_at, success, input_json
		FROM tool_calls
		WHERE workspace = ? AND session_id = ? AND called_at >= ?
		ORDER BY called_at`,
		workspace, sessionID, since.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("stats: tool calls for session: %w", err)
	}
	defer rows.Close()
	var out []Call
	for rows.Next() {
		var (
			c       Call
			calledM int64
			success int
		)
		if err := rows.Scan(&c.Tool, &calledM, &success, &c.InputJSON); err != nil {
			continue
		}
		c.Workspace = workspace
		c.SessionID = sessionID
		c.CalledAt = time.UnixMilli(calledM)
		c.Success = success != 0
		out = append(out, c)
	}
	return out, rows.Err()
}
