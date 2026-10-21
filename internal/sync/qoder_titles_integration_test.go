package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// Existing sessions get names and later renames without a transcript change.
func TestQoderFullSyncBackfillsTitles(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	require.Nil(t, qoderStoredTitle(t, archive, testjsonl.QoderTitleID))

	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Saved while closed")
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	assert.Zero(t, stats.Synced)
	assert.Equal(t, 1, stats.TitlesUpdated)
	require.Equal(t, new("Saved while closed"), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
	before, err := archive.GetSessionFull(t.Context(), "qoder:"+testjsonl.QoderTitleID)
	require.NoError(t, err)

	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Renamed in Qoder")
	// A watched transcript change triggers the sweep for the unchanged session.
	unrelated := testjsonl.WriteQoderTranscript(t, root, qoderOtherTitleID, "other session")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{unrelated}))
	after, err := archive.GetSessionFull(t.Context(), "qoder:"+testjsonl.QoderTitleID)
	require.NoError(t, err)
	assert.Equal(t, new("Renamed in Qoder"), after.SessionName)
	assert.Equal(t, 1, engine.LastSyncStats().TitlesUpdated)
	assert.Equal(t, before.FileHash, after.FileHash)
	assert.Equal(t, before.FilePath, after.FilePath)
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

func TestQoderTrashedSessionRestoresWithCurrentTitle(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	id := "qoder:" + testjsonl.QoderTitleID

	require.NoError(t, archive.SoftDeleteSession(t.Context(), id))
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Renamed in trash")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	_, err := archive.RestoreSession(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, new("Renamed in trash"), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
}

func TestQoderRebuildKeepsRenamesOfSessionsWithoutTranscript(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.Remove(transcript))
	testjsonl.WriteQoderTranscript(t, root,
		"22222222-3333-4333-9333-444444444444", "still here")
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Later")
	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)
	assert.Equal(t, new("Later"), qoderStoredTitle(t, archive, testjsonl.QoderTitleID),
		"an orphan copy must not restore an older title")
}

func TestQoderDisplayNameOutranksNativeTitle(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	id := "qoder:" + testjsonl.QoderTitleID
	require.NoError(t, archive.RenameSession(t.Context(), id, new("Renamed by user")))

	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Renamed in Qoder")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	session, err := archive.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, new("Renamed in Qoder"), session.SessionName,
		"the native title keeps tracking, so dropping the manual name reveals it")
	assert.Equal(t, new("Renamed by user"), session.DisplayName)
}

// TestQoderPrefixedEngineTitlesOnlyItsOwnRows pins that a store lookup always
// carries the engine's ID prefix, in both directions: a prefixed engine titles
// its own rows through the prefixed lookup, and neither engine reaches the
// other's row. Each engine ingests its own transcript, because one archive ends
// up holding only one row for a transcript both engines are given.
func TestQoderPrefixedEngineTitlesOnlyItsOwnRows(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	remoteTranscript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
	localTranscript := testjsonl.WriteQoderTranscript(t, root, qoderOtherTitleID, "other")
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "First")
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
	prefixed := "host-b:qoder:" + testjsonl.QoderTitleID
	require.Equal(t, new("First"), qoderTitleForID(t, archive, prefixed))
	require.Equal(t, new("First"), qoderStoredTitle(t, archive, qoderOtherTitleID))

	// A rename of the remote machine's row reaches only that row. Only the
	// store changed, so it can arrive through reconciliation and nothing else.
	// The local row carries a pending title in the store: a pass that reached
	// across the prefix would have somewhere to show up, and without it the
	// sweep writes the name the row already holds and stays invisible.
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Renamed on host-b")
	testjsonl.SetQoderTitle(t, store, qoderOtherTitleID, "Pending for host-b")
	require.NoError(t, remote.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Renamed on host-b"), qoderTitleForID(t, archive, prefixed),
		"a prefixed engine must reach its own row through the prefixed lookup")
	assert.Equal(t, new("First"), qoderStoredTitle(t, archive, qoderOtherTitleID),
		"a remote pass must not rename the local session")

	// And the other way, with the same pending trick on the remote row.
	testjsonl.SetQoderTitle(t, store, qoderOtherTitleID, "Renamed locally")
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Pending for host-a")
	require.NoError(t, local.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Renamed locally"), qoderStoredTitle(t, archive, qoderOtherTitleID))
	assert.Equal(t, new("Renamed on host-b"), qoderTitleForID(t, archive, prefixed),
		"a local pass must not rename another machine's session")
}

func TestQoderTitleFallbackOnReparse(t *testing.T) {
	for _, tc := range []struct {
		state, wantName string
		fileTitle       bool
	}{
		{"blank row", "Name in the app", true},
		{"removed row", "Updated export", true},
		{"unreadable file", "Name in the app", true},
		{"missing database no title", "Name in the app", false},
		{"removed row no title", "Name in the app", false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			root, store := testjsonl.QoderTitleRoot(t)
			transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
			if tc.fileTitle {
				require.NoError(t, os.WriteFile(strings.TrimSuffix(transcript, ".jsonl")+"-session.json", []byte(`{"title":"Name from export"}`), 0o644))
			}
			testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Name in the app")
			archive := dbtest.OpenTestDB(t)
			engine := newQoderTitleEngine(t, archive, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			require.Equal(t, new("Name in the app"), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
			original, err := os.ReadFile(store)
			require.NoError(t, err)
			switch tc.state {
			case "blank row":
				testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "   ")
			case "missing database no title":
				require.NoError(t, os.Remove(store))
			case "removed row", "removed row no title":
				conn, err := sql.Open("sqlite3", store)
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), "DELETE FROM chat_sessions WHERE session_id = ?", testjsonl.QoderTitleID)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
			case "unreadable file":
				require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o644))
			}
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			assert.Equal(t, new("Name in the app"), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
			transcript = testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "changed transcript")
			if tc.state == "removed row" {
				require.NoError(t, os.WriteFile(strings.TrimSuffix(transcript, ".jsonl")+"-session.json", []byte(`{"title":"Updated export"}`), 0o644))
			}
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			session, err := archive.GetSessionFull(t.Context(), "qoder:"+testjsonl.QoderTitleID)
			require.NoError(t, err)
			assert.Equal(t, new("changed transcript"), session.FirstMessage)
			assert.Equal(t, new(tc.wantName), session.SessionName)
			require.NoError(t, os.WriteFile(store, original, 0o600))
			testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Recovered rename")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			assert.Equal(t, new("Recovered rename"), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
		})
	}
}

func TestQoderMissingRowFollowsSidecarUpdates(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	testjsonl.SetQoderTitle(t, store, "other-session", "Other title")
	transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
	sidecar := strings.TrimSuffix(transcript, ".jsonl") + "-session.json"
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	for _, tc := range []struct {
		title, want string
	}{
		{"First", "First"},
		{"Later", "Later"},
		{"", "Later"},
	} {
		require.NoError(t, os.WriteFile(sidecar, []byte(`{"title":"`+tc.title+`"}`), 0o644))
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{sidecar, transcript}))
		assert.Equal(t, new(tc.want), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
	}
}
