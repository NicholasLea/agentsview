package testjsonl

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// QoderTitleID is a session ID shaped like the ones Qoder names transcripts
// with: the chat_sessions key is the transcript stem.
const QoderTitleID = "11111111-1111-4111-8111-111111111111"

// QoderProjectDir is the encoded project directory a Qoder transcript lives in.
const QoderProjectDir = "-Users-alice-sample-project"

// QoderTitleSchema is the smallest chat_sessions shape the title reader needs.
// The real table has more columns, and the two Qoder builds' column sets are
// not identical, so the reader must select only what it uses.
const QoderTitleSchema = `CREATE TABLE IF NOT EXISTS chat_sessions (
	session_id TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	cwd TEXT NOT NULL,
	session_kind TEXT,
	archived INTEGER,
	deleted_at TEXT
)`

// WriteQoderTranscript writes one session transcript under root and returns its
// path. The single user line is enough for the parser to emit one session.
func WriteQoderTranscript(t *testing.T, root, id, content string) string {
	t.Helper()
	path := filepath.Join(root, QoderProjectDir, id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	body := fmt.Sprintf(
		`{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:27.966Z","message":{"role":"user","content":%q},"sessionId":%q}`+"\n",
		content, id,
	)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// SetQoderTitle writes one title into the application database at store,
// creating the database and table on first use.
func SetQoderTitle(t *testing.T, store, id string, title any) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(store), 0o755))
	conn, err := sql.Open("sqlite3", store)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), QoderTitleSchema)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(),
		`INSERT INTO chat_sessions (session_id, title, cwd) VALUES (?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET title = excluded.title`,
		id, title, "/Users/alice/sample-project",
	)
	require.NoError(t, err)
}
