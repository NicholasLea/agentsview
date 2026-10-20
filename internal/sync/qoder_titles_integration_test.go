package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func qoderRequired(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("the Qoder title database path is macOS only")
	}
}

func newQoderTitleEngine(t *testing.T, archive *db.DB, roots ...string) *sync.Engine {
	t.Helper()
	return newQoderEngine(t, archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentQoder: roots},
		Machine:   "host-a",
	})
}

func newQoderEngine(t *testing.T, archive *db.DB, cfg sync.EngineConfig) *sync.Engine {
	t.Helper()
	engine := sync.NewEngine(t.Context(), archive, cfg)
	t.Cleanup(engine.Close)
	return engine
}

func qoderStoredTitle(t *testing.T, archive *db.DB, id string) *string {
	t.Helper()
	session, err := archive.GetSessionFull(t.Context(), "qoder:"+id)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session.SessionName
}

func TestQoderTitlesFollowStore(t *testing.T) {
	root, store := dbtest.QoderTitleRoot(t)
	transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
	dbtest.SetQoderTitle(t, store, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	require.Equal(t, new("First"), qoderStoredTitle(t, archive, dbtest.QoderTitleID))
	before, err := archive.GetSessionFull(t.Context(), "qoder:"+dbtest.QoderTitleID)
	require.NoError(t, err)

	dbtest.SetQoderTitle(t, store, "Renamed in Qoder")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	after, err := archive.GetSessionFull(t.Context(), "qoder:"+dbtest.QoderTitleID)
	require.NoError(t, err)
	assert.Equal(t, new("Renamed in Qoder"), after.SessionName)
	assert.Equal(t, 1, engine.LastSyncStats().TitlesUpdated)
	assert.Equal(t, before.FileHash, after.FileHash,
		"a rename must not disturb the transcript fingerprint")
	assert.Equal(t, before.FilePath, after.FilePath)
}

// TestQoderFullSyncBackfillsTitles covers sessions ingested while Qoder held
// the database busy, and a store created only after AgentsView started.
func TestQoderFullSyncBackfillsTitles(t *testing.T) {
	root, store := dbtest.QoderTitleRoot(t)
	transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	require.Nil(t, qoderStoredTitle(t, archive, dbtest.QoderTitleID))

	dbtest.SetQoderTitle(t, store, "Saved while closed")
	engine.SyncAll(t.Context(), nil)
	assert.Equal(t, new("Saved while closed"),
		qoderStoredTitle(t, archive, dbtest.QoderTitleID),
		"existing sessions pick up titles without a transcript change")
}

// TestQoderBlankTitleKeepsStoredName pins the deliberate difference from
// Antigravity: a blank chat_sessions title carries no signal, so it never
// erases a name the sidecar or an earlier read supplied.
func TestQoderBlankTitleKeepsStoredName(t *testing.T) {
	root, store := dbtest.QoderTitleRoot(t)
	transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
	dbtest.SetQoderTitle(t, store, "Named")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	require.Equal(t, new("Named"), qoderStoredTitle(t, archive, dbtest.QoderTitleID))

	dbtest.SetQoderTitle(t, store, "   ")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Named"), qoderStoredTitle(t, archive, dbtest.QoderTitleID))
}

func TestQoderUnreadableTitleStoreKeepsTitle(t *testing.T) {
	for name, schema := range map[string]string{
		"missing_table":  "CREATE TABLE other (title TEXT)",
		"missing_column": "CREATE TABLE chat_sessions (session_id TEXT PRIMARY KEY, name TEXT)",
	} {
		t.Run(name, func(t *testing.T) {
			root, store := dbtest.QoderTitleRoot(t)
			transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
			dbtest.SetQoderTitle(t, store, "First")
			archive := dbtest.OpenTestDB(t)
			engine := newQoderTitleEngine(t, archive, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

			incompatible := filepath.Join(t.TempDir(), parser.QoderTitleDBName)
			conn, err := sql.Open("sqlite3", incompatible)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), schema)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			data, err := os.ReadFile(incompatible)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(store, data, 0o644))
			engine.SyncAll(t.Context(), nil)
			assert.Equal(t, new("First"), qoderStoredTitle(t, archive, dbtest.QoderTitleID),
				"an unreadable store leaves the stored title alone")
		})
	}
}

// TestQoderStoreTitlesOnlySessionsItOwns keeps one root's database from
// renaming a session that another root's copy of the same ID belongs to.
func TestQoderStoreTitlesOnlySessionsItOwns(t *testing.T) {
	qoderRequired(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	bound := filepath.Join(home, ".qoder", "projects")
	require.NoError(t, os.MkdirAll(bound, 0o755))
	store, ok := parser.QoderTitleDatabase(bound)
	require.True(t, ok)
	unbound := filepath.Join(home, "Library", "Application Support",
		"Qoder", "SharedClientCache", "cli", "projects")
	require.NoError(t, os.MkdirAll(unbound, 0o755))

	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "From bound root")
	transcript := testjsonl.WriteQoderTranscript(t, unbound, testjsonl.QoderTitleID, "hello")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, bound, unbound)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	assert.Nil(t, qoderStoredTitle(t, archive, testjsonl.QoderTitleID),
		"a root with no title database ingests untitled")

	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Nil(t, qoderStoredTitle(t, archive, testjsonl.QoderTitleID),
		"the bound root's store must not rename a session it does not own")
	assert.Zero(t, engine.LastSyncStats().TitlesUpdated)
}

func TestQoderTitleRenamesRespectCwdFilter(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "excluded", want: "First"},
		{name: "allowed", want: "Later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, store := dbtest.QoderTitleRoot(t)
			workspace := t.TempDir()
			transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
			dbtest.SetQoderTitle(t, store, "First")
			archive := dbtest.OpenTestDB(t)
			require.NoError(t, newQoderTitleEngine(t, archive, root).
				SyncPathsContext(t.Context(), []string{transcript}))
			require.NoError(t, archive.UpdateSessionCwd(t.Context(),
				"qoder:"+dbtest.QoderTitleID, filepath.Join(workspace, tc.name)))
			allowed := filepath.Join(workspace, "allowed")
			engine := newQoderEngine(t, archive, sync.EngineConfig{
				AgentDirs:          map[parser.AgentType][]string{parser.AgentQoder: {root}},
				Machine:            "host-a",
				IncludeCwdPrefixes: []string{allowed},
			})
			dbtest.SetQoderTitle(t, store, "Later")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			assert.Equal(t, new(tc.want), qoderStoredTitle(t, archive, dbtest.QoderTitleID))
		})
	}
}

func TestQoderTrashedSessionRestoresWithCurrentTitle(t *testing.T) {
	root, store := dbtest.QoderTitleRoot(t)
	transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
	dbtest.SetQoderTitle(t, store, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	id := "qoder:" + dbtest.QoderTitleID

	require.NoError(t, archive.SoftDeleteSession(t.Context(), id))
	dbtest.SetQoderTitle(t, store, "Renamed in trash")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	_, err := archive.RestoreSession(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, new("Renamed in trash"), qoderStoredTitle(t, archive, dbtest.QoderTitleID))
}

func TestQoderRebuildKeepsRenamesOfSessionsWithoutTranscript(t *testing.T) {
	root, store := dbtest.QoderTitleRoot(t)
	transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
	dbtest.SetQoderTitle(t, store, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.Remove(transcript))
	testjsonl.WriteQoderTranscript(t, root,
		"22222222-3333-4333-9333-444444444444", "still here")
	dbtest.SetQoderTitle(t, store, "Later")
	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)
	assert.Equal(t, new("Later"), qoderStoredTitle(t, archive, dbtest.QoderTitleID),
		"an orphan copy must not restore an older title")
}

func TestQoderDisplayNameOutranksNativeTitle(t *testing.T) {
	root, store := dbtest.QoderTitleRoot(t)
	transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
	dbtest.SetQoderTitle(t, store, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	id := "qoder:" + dbtest.QoderTitleID
	require.NoError(t, archive.RenameSession(t.Context(), id, new("Renamed by user")))

	dbtest.SetQoderTitle(t, store, "Renamed in Qoder")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	session, err := archive.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, new("Renamed in Qoder"), session.SessionName,
		"the native title keeps tracking, so dropping the manual name reveals it")
	assert.Equal(t, new("Renamed by user"), session.DisplayName)
}

// TestQoderBlankRowDoesNotLoseStoredNameToSidecar covers the one blank-title
// case reconciliation cannot heal: the app held a title, its row goes blank,
// and an older export snapshot is still beside the transcript.
func TestQoderBlankRowDoesNotLoseStoredNameToSidecar(t *testing.T) {
	root, store := dbtest.QoderTitleRoot(t)
	transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
	require.NoError(t, os.WriteFile(
		strings.TrimSuffix(transcript, ".jsonl")+"-session.json",
		[]byte(`{"title":"Name from export"}`), 0o644,
	))
	dbtest.SetQoderTitle(t, store, "Name in the app")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	id := "qoder:" + dbtest.QoderTitleID
	before, err := archive.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, new("Name in the app"), before.SessionName)

	dbtest.SetQoderTitle(t, store, "   ")
	body, err := os.ReadFile(transcript)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(transcript,
		append(body, []byte(`{"type":"user","uuid":"u2","timestamp":"2026-06-04T09:48:27.966Z","message":{"role":"user","content":"keep going"},"sessionId":"`+
			dbtest.QoderTitleID+`"}`+"\n")...), 0o644))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	after, err := archive.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, new("Name in the app"), after.SessionName,
		"a blank row must not hand the name back to the export snapshot")
	assert.NotEqual(t, before.FileHash, after.FileHash, "the transcript really reparsed")
}

// TestQoderUnreadableStoreSelfHeals pins the deliberate half of this contract:
// an unreadable database is left as "no signal" rather than cleared, so an
// export snapshot can show for one pass and the stored name comes back once the
// database reads again. Clearing on a read failure would cost a title to a
// session imported during a lock, and reconciliation already retries.
func TestQoderUnreadableStoreSelfHeals(t *testing.T) {
	root, store := dbtest.QoderTitleRoot(t)
	transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
	require.NoError(t, os.WriteFile(
		strings.TrimSuffix(transcript, ".jsonl")+"-session.json",
		[]byte(`{"title":"Name from export"}`), 0o644,
	))
	dbtest.SetQoderTitle(t, store, "Name in the app")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	require.Equal(t, new("Name in the app"), qoderStoredTitle(t, archive, dbtest.QoderTitleID))

	original, err := os.ReadFile(store)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o644))
	body, err := os.ReadFile(transcript)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(transcript,
		append(body, []byte(`{"type":"user","uuid":"u2","timestamp":"2026-06-04T09:48:27.966Z","message":{"role":"user","content":"keep going"},"sessionId":"`+
			dbtest.QoderTitleID+`"}`+"\n")...), 0o644))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	assert.Equal(t, new("Name from export"), qoderStoredTitle(t, archive, dbtest.QoderTitleID),
		"a database that cannot be read lets the export snapshot through")

	require.NoError(t, os.WriteFile(store, original, 0o600))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Name in the app"), qoderStoredTitle(t, archive, dbtest.QoderTitleID),
		"the next pass that reads the database puts the stored name back")
}

func TestQoderDisabledProvidersStayIndependent(t *testing.T) {
	qoderRoot, qoderStore := dbtest.QoderTitleRoot(t)
	antigravityRoot := t.TempDir()
	qoderTranscript := dbtest.WriteQoderTitleTranscript(t, qoderRoot, "hello")
	antigravityTranscript := dbtest.WriteAntigravityTitleTranscript(t, antigravityRoot, "hello")
	dbtest.SetQoderTitle(t, qoderStore, "Qoder title")
	dbtest.SetAntigravityTitle(t, antigravityRoot, "Antigravity title")
	archive := dbtest.OpenTestDB(t)
	both := map[parser.AgentType][]string{
		parser.AgentQoder: {qoderRoot}, parser.AgentAntigravity: {antigravityRoot},
	}
	require.NoError(t, newQoderEngine(t, archive, sync.EngineConfig{
		AgentDirs: both, Machine: "host-a",
	}).SyncPathsContext(t.Context(), []string{qoderTranscript, antigravityTranscript}))

	t.Run("antigravity disabled", func(t *testing.T) {
		engine := newQoderEngine(t, archive, sync.EngineConfig{
			AgentDirs:      both,
			Machine:        "host-a",
			DisabledAgents: []parser.AgentType{parser.AgentAntigravity},
		})
		store := dbtest.SetQoderTitle(t, qoderStore, "Qoder renamed")
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
		assert.Equal(t, new("Qoder renamed"),
			qoderStoredTitle(t, archive, dbtest.QoderTitleID),
			"disabling Antigravity must not stop Qoder titles")
	})
	t.Run("qoder disabled", func(t *testing.T) {
		engine := newQoderEngine(t, archive, sync.EngineConfig{
			AgentDirs:      both,
			Machine:        "host-a",
			DisabledAgents: []parser.AgentType{parser.AgentQoder},
		})
		store := dbtest.SetAntigravityTitle(t, antigravityRoot, "Antigravity renamed")
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
		assert.Equal(t, new("Antigravity renamed"),
			antigravityStoredTitle(t, archive),
			"disabling Qoder must not stop Antigravity titles")
	})
}

// TestQoderPrefixedEngineTitlesOnlyItsOwnRows pins that a store lookup always
// carries the engine's ID prefix, so another machine's rows are never matched.
func TestQoderPrefixedEngineTitlesOnlyItsOwnRows(t *testing.T) {
	root, store := dbtest.QoderTitleRoot(t)
	transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
	dbtest.SetQoderTitle(t, store, "First")
	archive := dbtest.OpenTestDB(t)
	remote := newQoderEngine(t, archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentQoder: {root}},
		Machine:   "host-b",
		IDPrefix:  "host-b:",
	})
	require.NoError(t, remote.SyncPathsContext(t.Context(), []string{transcript}))
	prefixed := "host-b:qoder:" + dbtest.QoderTitleID
	session, err := archive.GetSessionFull(t.Context(), prefixed)
	require.NoError(t, err)
	require.NotNil(t, session)

	local := newQoderTitleEngine(t, archive, root)
	dbtest.SetQoderTitle(t, store, "Later")
	require.NoError(t, local.SyncPathsContext(t.Context(), []string{store}))
	renamed, err := archive.GetSessionFull(t.Context(), prefixed)
	require.NoError(t, err)
	assert.NotEqual(t, new("Later"), renamed.SessionName,
		"a local store must not rename another machine's session")
}

func TestQoderUsageOnlyArchiveSkipsTitleStore(t *testing.T) {
	root, store := dbtest.QoderTitleRoot(t)
	transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
	dbtest.SetQoderTitle(t, store, "First")
	archive := dbtest.OpenTestDB(t)
	archive.SetArchiveContent(config.ArchiveContentUsage)
	require.NoError(t, newQoderTitleEngine(t, archive, root).
		SyncPathsContext(t.Context(), []string{transcript}))

	// An unreadable store would be reported if the pass read it at all. A fresh
	// engine keeps the failure count observable rather than inherited.
	require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o644))
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Zero(t, engine.LastSyncStats().Failed,
		"a usage-only pass is a no-op and never reads the store")
}

// TestQoderRemovedRowKeepsStoredName covers the second half of the "no usable
// database title" rule: the row is gone entirely, so reconciliation has nothing
// to restore from and the export snapshot must not take its place.
func TestQoderRemovedRowKeepsStoredName(t *testing.T) {
	root, store := dbtest.QoderTitleRoot(t)
	transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
	require.NoError(t, os.WriteFile(
		strings.TrimSuffix(transcript, ".jsonl")+"-session.json",
		[]byte(`{"title":"Name from export"}`), 0o644,
	))
	dbtest.SetQoderTitle(t, store, "Name in the app")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	require.Equal(t, new("Name in the app"), qoderStoredTitle(t, archive, dbtest.QoderTitleID))

	conn, err := sql.Open("sqlite3", store)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(),
		"DELETE FROM chat_sessions WHERE session_id = ?", dbtest.QoderTitleID)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	body, err := os.ReadFile(transcript)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(transcript,
		append(body, []byte(`{"type":"user","uuid":"u2","timestamp":"2026-06-04T09:48:27.966Z","message":{"role":"user","content":"keep going"},"sessionId":"`+
			dbtest.QoderTitleID+`"}`+"\n")...), 0o644))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	assert.Equal(t, new("Name in the app"), qoderStoredTitle(t, archive, dbtest.QoderTitleID),
		"a removed row must not hand the name back to the export snapshot")
}
