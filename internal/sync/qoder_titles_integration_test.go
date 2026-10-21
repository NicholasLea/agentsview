package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
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

// qoderOtherTitleID is a second application-database row, used where one engine
// must not see the session owned by another.
const qoderOtherTitleID = "22222222-2222-4222-8222-222222222222"

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

func qoderTitleForID(t *testing.T, archive *db.DB, id string) *string {
	t.Helper()
	session, err := archive.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session.SessionName
}

func qoderStoredTitle(t *testing.T, archive *db.DB, id string) *string {
	t.Helper()
	return qoderTitleForID(t, archive, "qoder:"+id)
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

// TestQoderStoreTitlesOnlySessionsItOwns keeps one root's database from
// renaming a session that another root's copy of the same ID belongs to.
func TestQoderStoreTitlesOnlySessionsItOwns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
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
// carries the engine's ID prefix, in both directions: a prefixed engine titles
// its own rows through the prefixed lookup, and neither engine reaches the
// other's row. Each engine ingests its own transcript, because one archive ends
// up holding only one row for a transcript both engines are given.
func TestQoderPrefixedEngineTitlesOnlyItsOwnRows(t *testing.T) {
	root, store := dbtest.QoderTitleRoot(t)
	remoteTranscript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
	localTranscript := testjsonl.WriteQoderTranscript(t, root, qoderOtherTitleID, "other")
	testjsonl.SetQoderTitle(t, store, dbtest.QoderTitleID, "First")
	testjsonl.SetQoderTitle(t, store, qoderOtherTitleID, "First")
	archive := dbtest.OpenTestDB(t)
	remote := newQoderEngine(t, archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentQoder: {root}},
		Machine:   "host-b",
		IDPrefix:  "host-b:",
	})
	require.NoError(t, remote.SyncPathsContext(t.Context(), []string{remoteTranscript}))
	local := newQoderTitleEngine(t, archive, root)
	require.NoError(t, local.SyncPathsContext(t.Context(), []string{localTranscript}))
	prefixed := "host-b:qoder:" + dbtest.QoderTitleID
	require.Equal(t, new("First"), qoderTitleForID(t, archive, prefixed))
	require.Equal(t, new("First"), qoderStoredTitle(t, archive, qoderOtherTitleID))

	// A rename of the remote machine's row reaches only that row. Only the
	// store changed, so it can arrive through reconciliation and nothing else.
	// The local row carries a pending title in the store: a pass that reached
	// across the prefix would have somewhere to show up, and without it the
	// sweep writes the name the row already holds and stays invisible.
	testjsonl.SetQoderTitle(t, store, dbtest.QoderTitleID, "Renamed on host-b")
	testjsonl.SetQoderTitle(t, store, qoderOtherTitleID, "Pending for host-b")
	require.NoError(t, remote.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Renamed on host-b"), qoderTitleForID(t, archive, prefixed),
		"a prefixed engine must reach its own row through the prefixed lookup")
	assert.Equal(t, new("First"), qoderStoredTitle(t, archive, qoderOtherTitleID),
		"a remote pass must not rename the local session")

	// And the other way, with the same pending trick on the remote row.
	testjsonl.SetQoderTitle(t, store, qoderOtherTitleID, "Renamed locally")
	testjsonl.SetQoderTitle(t, store, dbtest.QoderTitleID, "Pending for host-a")
	require.NoError(t, local.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Renamed locally"), qoderStoredTitle(t, archive, qoderOtherTitleID))
	assert.Equal(t, new("Renamed on host-b"), qoderTitleForID(t, archive, prefixed),
		"a local pass must not rename another machine's session")
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

func TestQoderTitleFallbackOnReparse(t *testing.T) {
	for _, tc := range []struct {
		state, wantName string
	}{
		{"blank row", "Name in the app"},
		{"removed row", "Updated export"},
		{"unreadable file", "Name in the app"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			root, store := dbtest.QoderTitleRoot(t)
			transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
			require.NoError(t, os.WriteFile(strings.TrimSuffix(transcript, ".jsonl")+"-session.json", []byte(`{"title":"Name from export"}`), 0o644))
			dbtest.SetQoderTitle(t, store, "Name in the app")
			archive := dbtest.OpenTestDB(t)
			engine := newQoderTitleEngine(t, archive, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			require.Equal(t, new("Name in the app"), qoderStoredTitle(t, archive, dbtest.QoderTitleID))
			original, err := os.ReadFile(store)
			require.NoError(t, err)
			switch tc.state {
			case "blank row":
				dbtest.SetQoderTitle(t, store, "   ")
			case "removed row":
				conn, err := sql.Open("sqlite3", store)
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), "DELETE FROM chat_sessions WHERE session_id = ?", dbtest.QoderTitleID)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
			case "unreadable file":
				require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o644))
			}
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			assert.Equal(t, new("Name in the app"), qoderStoredTitle(t, archive, dbtest.QoderTitleID))
			transcript = dbtest.WriteQoderTitleTranscript(t, root, "changed transcript")
			require.NoError(t, os.WriteFile(strings.TrimSuffix(transcript, ".jsonl")+"-session.json", []byte(`{"title":"Updated export"}`), 0o644))
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			session, err := archive.GetSessionFull(t.Context(), "qoder:"+dbtest.QoderTitleID)
			require.NoError(t, err)
			assert.Equal(t, new("changed transcript"), session.FirstMessage)
			assert.Equal(t, new(tc.wantName), session.SessionName)
			require.NoError(t, os.WriteFile(store, original, 0o600))
			dbtest.SetQoderTitle(t, store, "Recovered rename")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			assert.Equal(t, new("Recovered rename"), qoderStoredTitle(t, archive, dbtest.QoderTitleID))
		})
	}
}

func TestQoderMissingDatabaseFollowsSidecarUpdates(t *testing.T) {
	root, _ := dbtest.QoderTitleRoot(t)
	transcript := dbtest.WriteQoderTitleTranscript(t, root, "hello")
	sidecar := strings.TrimSuffix(transcript, ".jsonl") + "-session.json"
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	for _, title := range []string{"First", "Renamed", ""} {
		require.NoError(t, os.WriteFile(sidecar, []byte(`{"title":"`+title+`"}`), 0o644))
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{sidecar, transcript}))
		if title == "" {
			assert.Nil(t, qoderStoredTitle(t, archive, dbtest.QoderTitleID))
		} else {
			assert.Equal(t, &title, qoderStoredTitle(t, archive, dbtest.QoderTitleID))
		}
	}
}
