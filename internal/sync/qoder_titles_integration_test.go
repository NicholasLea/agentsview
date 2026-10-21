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

func newQoderTitleEngine(t *testing.T, archive *db.DB, roots ...string) *sync.Engine {
	t.Helper()
	cfg := sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentQoder: roots},
		Machine:   "host-a",
	}
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

func TestQoderTitleFallbackOnReparse(t *testing.T) {
	for _, tc := range []struct {
		state, wantName string
		fileTitle       bool
	}{
		{"blank row", "Name in the app", true},
		{"removed row", "Updated export", true},
		{"unreadable file", "Name in the app", true},
		{"missing database no title", "Name in the app", false},
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
			var original []byte
			if tc.state == "unreadable file" {
				var err error
				original, err = os.ReadFile(store)
				require.NoError(t, err)
			}
			switch tc.state {
			case "blank row":
				testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "   ")
			case "missing database no title":
				require.NoError(t, os.Remove(store))
			case "removed row":
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
			if tc.state == "unreadable file" {
				require.NoError(t, os.WriteFile(store, original, 0o600))
				testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Recovered rename")
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
				assert.Equal(t, new("Recovered rename"), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
			}
		})
	}
}

func TestQoderMissingRowFollowsSidecarUpdates(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(store), 0o755))
	conn, err := sql.Open("sqlite3", store)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), testjsonl.QoderTitleSchema)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
	sidecar := strings.TrimSuffix(transcript, ".jsonl") + "-session.json"
	archive := dbtest.OpenTestDB(t)
	engine := newQoderTitleEngine(t, archive, root)
	for _, tc := range []struct {
		title, want string
	}{
		{title: "First", want: "First"},
		{title: "Later", want: "Later"},
		{title: "", want: "Later"},
	} {
		require.NoError(t, os.WriteFile(sidecar, []byte(`{"title":"`+tc.title+`"}`), 0o644))
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{sidecar, transcript}))
		assert.Zero(t, engine.LastSyncStats().Failed)
		assert.Equal(t, new(tc.want), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
	}
	stats := engine.SyncAll(t.Context(), nil)
	assert.Zero(t, stats.Failed)
	assert.Equal(t, new("Later"), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
}
