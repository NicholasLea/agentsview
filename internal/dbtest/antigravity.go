package dbtest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

const AntigravityTitleID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func WriteAntigravityTitleTranscript(t *testing.T, root, content string) string {
	t.Helper()
	path := filepath.Join(root, "brain", AntigravityTitleID, ".system_generated", "logs", "transcript.jsonl")
	WriteTestFile(t, path, []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"`+content+`"}`+"\n"))
	return path
}

func SetAntigravityTitle(t *testing.T, root string, title any) string {
	t.Helper()
	path := filepath.Join(root, parser.AntigravitySummariesDBName)
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`CREATE TABLE IF NOT EXISTS conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT)`)
	require.NoError(t, err)
	_, err = conn.Exec(`INSERT INTO conversation_summaries VALUES (?, ?) ON CONFLICT(conversation_id) DO UPDATE SET title = excluded.title`, AntigravityTitleID, title)
	require.NoError(t, err)
	return path
}
