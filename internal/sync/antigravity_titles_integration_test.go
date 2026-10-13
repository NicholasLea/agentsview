package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

const antigravityTitleID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func writeAntigravityTitleTranscript(t *testing.T, root, content string) string {
	t.Helper()
	path := filepath.Join(root, "brain", antigravityTitleID, ".system_generated", "logs", "transcript.jsonl")
	dbtest.WriteTestFile(t, path, []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"`+content+`"}`+"\n"))
	return path
}

func setAntigravityTitle(t *testing.T, root string, title any) string {
	t.Helper()
	path := filepath.Join(root, parser.AntigravitySummariesDBName)
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`CREATE TABLE IF NOT EXISTS conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT)`)
	require.NoError(t, err)
	_, err = conn.Exec(`INSERT INTO conversation_summaries VALUES (?, ?) ON CONFLICT(conversation_id) DO UPDATE SET title = excluded.title`, antigravityTitleID, title)
	require.NoError(t, err)
	return path
}

func newAntigravityTitleEngine(t *testing.T, archive *db.DB, roots ...string) *sync.Engine {
	t.Helper()
	return sync.NewEngine(t.Context(), archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: roots}, Machine: "host-a",
	})
}

func antigravityStoredTitle(t *testing.T, archive *db.DB) *string {
	t.Helper()
	session, err := archive.GetSessionFull(t.Context(), "antigravity:"+antigravityTitleID)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session.SessionName
}

func TestAntigravityTitlesFollowStoreWithoutReparse(t *testing.T) {
	root := t.TempDir()
	transcript := writeAntigravityTitleTranscript(t, root, "hello")
	store := setAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, root)

	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	require.Equal(t, new("First"), antigravityStoredTitle(t, archive), "a new session takes its saved title")
	before, err := archive.GetSessionFull(t.Context(), "antigravity:"+antigravityTitleID)
	require.NoError(t, err)

	setAntigravityTitle(t, root, "Second")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	after, err := archive.GetSessionFull(t.Context(), "antigravity:"+antigravityTitleID)
	require.NoError(t, err)
	assert.Equal(t, new("Second"), after.SessionName, "a store-only rename updates the title")
	assert.Equal(t, before.FileHash, after.FileHash, "a rename leaves the transcript fingerprint alone")

	setAntigravityTitle(t, root, "  ")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Nil(t, antigravityStoredTitle(t, archive), "a blank saved title clears the name")
}

func TestAntigravityTitlesReachSessionsWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	transcript := writeAntigravityTitleTranscript(t, root, "hello")
	store := setAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	setAntigravityTitle(t, root, "Renamed")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Renamed"), antigravityStoredTitle(t, archive))
}

func TestAntigravityUnreadableTitleStoreKeepsTitle(t *testing.T) {
	root := t.TempDir()
	transcript := writeAntigravityTitleTranscript(t, root, "hello")
	store := setAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o644))
	writeAntigravityTitleTranscript(t, root, "hello again")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, store}))
	engine.SyncAll(t.Context(), nil)
	assert.Equal(t, new("First"), antigravityStoredTitle(t, archive), "an unreadable store keeps the stored title")
}

func TestAntigravityFullSyncBackfillsTitles(t *testing.T) {
	root := t.TempDir()
	transcript := writeAntigravityTitleTranscript(t, root, "hello")
	archive := dbtest.OpenTestDB(t)
	require.NoError(t, newAntigravityTitleEngine(t, archive, root).SyncPathsContext(t.Context(), []string{transcript}))
	require.Nil(t, antigravityStoredTitle(t, archive))

	setAntigravityTitle(t, root, "Saved while closed")
	newAntigravityTitleEngine(t, archive, root).SyncAll(t.Context(), nil)
	assert.Equal(t, new("Saved while closed"), antigravityStoredTitle(t, archive), "existing sessions pick up titles without a transcript change")
}

func TestAntigravityTitleStoreRenamesOnlyItsOwnSessions(t *testing.T) {
	owner, other := t.TempDir(), t.TempDir()
	transcript := writeAntigravityTitleTranscript(t, owner, "hello")
	setAntigravityTitle(t, owner, "Owner title")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, owner, other)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	store := setAntigravityTitle(t, other, "Other root title")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Owner title"), antigravityStoredTitle(t, archive))
}

func TestAntigravityRebuildKeepsRenamesOfSessionsWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	transcript := writeAntigravityTitleTranscript(t, root, "hello")
	setAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
	// A rebuild with no live sources refuses to swap.
	dbtest.WriteTestFile(t, filepath.Join(root, "brain", "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee", ".system_generated", "logs", "transcript.jsonl"),
		[]byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"live"}`+"\n"))
	setAntigravityTitle(t, root, "Later")
	engine.ResyncAll(t.Context(), nil)
	assert.Equal(t, new("Later"), antigravityStoredTitle(t, archive), "orphan copies must not restore an older title")
}
