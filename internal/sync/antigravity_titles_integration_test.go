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

func newAntigravityTitleEngine(t *testing.T, archive *db.DB, roots ...string) *sync.Engine {
	t.Helper()
	return sync.NewEngine(t.Context(), archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: roots}, Machine: "host-a",
	})
}

func antigravityStoredTitle(t *testing.T, archive *db.DB) *string {
	t.Helper()
	session, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session.SessionName
}

func TestAntigravityTitlesFollowStoreWithoutReparse(t *testing.T) {
	for _, mode := range []string{"brain", "database"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			if mode == "database" {
				dbPath := filepath.Join(root, "conversations", dbtest.AntigravityTitleID+".db")
				require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
				copyAntigravityCLITestSchemaTemplate(t, dbPath)
				transcript = dbPath
			}
			store := dbtest.SetAntigravityTitle(t, root, "First")
			archive := dbtest.OpenTestDB(t)
			engine := newAntigravityTitleEngine(t, archive, root)

			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			require.Equal(t, new("First"), antigravityStoredTitle(t, archive), "a new session takes its saved title")
			before, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
			require.NoError(t, err)
			assert.Equal(t, new(transcript), before.FilePath)

			dbtest.SetAntigravityTitle(t, root, "Second")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			after, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
			require.NoError(t, err)
			assert.Equal(t, new("Second"), after.SessionName, "a store-only rename updates the title")
			assert.Equal(t, 1, engine.LastSyncStats().TitlesUpdated)
			assert.Equal(t, before.FileHash, after.FileHash, "a rename leaves the transcript fingerprint alone")

			dbtest.SetAntigravityTitle(t, root, "  ")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			assert.Nil(t, antigravityStoredTitle(t, archive), "a blank saved title clears the name")
			for _, tc := range []struct {
				title   string
				want    *string
				updates int
			}{
				{title: "ab", want: new("ab"), updates: 1},
				{title: "a\x00b", want: new("ab"), updates: 0},
				{title: "\x00", want: nil, updates: 1},
				{title: "\x00\x00", want: nil, updates: 0},
			} {
				dbtest.SetAntigravityTitle(t, root, tc.title)
				plan, err := engine.PlanChangedPathsContext(t.Context(), []string{store})
				require.NoError(t, err)
				result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
				require.NoError(t, err)
				assert.Equal(t, tc.updates, result.Stats.TitlesUpdated)
				assert.Equal(t, tc.want, antigravityStoredTitle(t, archive))
			}
		})
	}
}

func TestAntigravityTitlesReachSessionsWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	store := dbtest.SetAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	dbtest.SetAntigravityTitle(t, root, "Renamed")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Renamed"), antigravityStoredTitle(t, archive))
}

func TestAntigravityUnreadableTitleStoreKeepsTitle(t *testing.T) {
	for name, schema := range map[string]string{
		"corrupt":        "",
		"missing_table":  "CREATE TABLE other (title TEXT)",
		"missing_column": "CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY)",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			store := dbtest.SetAntigravityTitle(t, root, "First")
			archive := dbtest.OpenTestDB(t)
			engine := newAntigravityTitleEngine(t, archive, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

			data := []byte("not a database")
			if schema != "" {
				incompatible := filepath.Join(t.TempDir(), parser.AntigravitySummariesDBName)
				conn, err := sql.Open("sqlite3", incompatible)
				require.NoError(t, err)
				_, err = conn.Exec(schema)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
				data, err = os.ReadFile(incompatible)
				require.NoError(t, err)
			}
			require.NoError(t, os.WriteFile(store, data, 0o644))
			parser.InvalidateAntigravityTitles(store)
			_, err := parser.AntigravityStoreTitles(t.Context(), store)
			assert.Error(t, err)
			dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, store}))
			engine.SyncAll(t.Context(), nil)
			assert.Equal(t, new("First"), antigravityStoredTitle(t, archive), "an unreadable store keeps the stored title")
		})
	}
}

func TestAntigravityFullSyncBackfillsTitles(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	archive := dbtest.OpenTestDB(t)
	require.NoError(t, newAntigravityTitleEngine(t, archive, root).SyncPathsContext(t.Context(), []string{transcript}))
	require.Nil(t, antigravityStoredTitle(t, archive))

	dbtest.SetAntigravityTitle(t, root, "Saved while closed")
	newAntigravityTitleEngine(t, archive, root).SyncAll(t.Context(), nil)
	assert.Equal(t, new("Saved while closed"), antigravityStoredTitle(t, archive), "existing sessions pick up titles without a transcript change")
}

func TestAntigravityTitleStoreRenamesOnlyItsOwnSessions(t *testing.T) {
	owner, other := t.TempDir(), t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, owner, "hello")
	dbtest.SetAntigravityTitle(t, owner, "Owner title")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, owner, other)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	store := dbtest.SetAntigravityTitle(t, other, "Other root title")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Owner title"), antigravityStoredTitle(t, archive))
}

func TestAntigravityRebuildKeepsRenamesOfSessionsWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	dbtest.SetAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
	// A rebuild with no live sources refuses to swap.
	dbtest.WriteTestFile(t, filepath.Join(root, "brain", "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee", ".system_generated", "logs", "transcript.jsonl"),
		[]byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"live"}`+"\n"))
	dbtest.SetAntigravityTitle(t, root, "Later")
	engine.ResyncAll(t.Context(), nil)
	assert.Equal(t, new("Later"), antigravityStoredTitle(t, archive), "orphan copies must not restore an older title")
}

func TestAntigravityTrashedSessionRestoresWithCurrentTitle(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	store := dbtest.SetAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	id := "antigravity:" + dbtest.AntigravityTitleID

	require.NoError(t, archive.SoftDeleteSession(t.Context(), id))
	dbtest.SetAntigravityTitle(t, root, "Renamed in trash")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	_, err := archive.RestoreSession(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, new("Renamed in trash"), antigravityStoredTitle(t, archive))
}

func TestAntigravityTitleWriteFailureRetriesAndKeepsSyncStatus(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	store := dbtest.SetAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	beforeSync, beforeStats := engine.LastSync(), engine.LastSyncStats()
	unrelated := filepath.Join(t.TempDir(), "unrelated")
	require.NoError(t, os.WriteFile(unrelated, []byte("unrelated"), 0o644))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{unrelated}))
	assert.Equal(t, beforeSync, engine.LastSync())
	assert.Equal(t, beforeStats, engine.LastSyncStats())
	plan, err := engine.PlanChangedPathsContext(t.Context(), []string{unrelated})
	require.NoError(t, err)
	_, err = engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
	require.NoError(t, err)
	assert.Equal(t, beforeSync, engine.LastSync())
	assert.Equal(t, beforeStats, engine.LastSyncStats())
	raw, err := sql.Open("sqlite3", archive.Path())
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.Exec(`CREATE TRIGGER fail_title_write BEFORE UPDATE OF session_name ON sessions BEGIN SELECT RAISE(FAIL, 'injected title write failure'); END`)
	require.NoError(t, err)
	dbtest.SetAntigravityTitle(t, root, "Later")
	plan, err = engine.PlanChangedPathsContext(t.Context(), []string{store})
	require.NoError(t, err)
	result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
	require.ErrorContains(t, err, "injected title write failure")
	assert.Equal(t, 1, result.Stats.Failed)
	assert.False(t, result.Stats.ProcessingComplete())
	assert.Empty(t, engine.SnapshotAntigravityTitleStates())
	assert.Equal(t, new("First"), antigravityStoredTitle(t, archive))
	assert.Equal(t, beforeSync, engine.LastSync())
	_, err = raw.Exec("DROP TRIGGER fail_title_write")
	require.NoError(t, err)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{unrelated}))
	assert.Equal(t, new("Later"), antigravityStoredTitle(t, archive))
	assert.Equal(t, beforeStats.Synced, engine.LastSyncStats().Synced)
	assert.Equal(t, beforeStats.TitlesUpdated+1, engine.LastSyncStats().TitlesUpdated)
	assert.True(t, engine.LastSync().After(beforeSync))
}
