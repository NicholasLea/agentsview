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
		symlink         bool
	}{
		{"database rename", "Renamed in the app", false},
		{"database rename", "Renamed in the app", true},
		{"blank row", "Name in the app", false},
		{"unreadable file", "Name in the app", false},
	} {
		name := tc.state
		if tc.symlink {
			name += " symlinked default root"
		}
		t.Run(name, func(t *testing.T) {
			root, store := testjsonl.QoderTitleRoot(t)
			if tc.symlink {
				target := t.TempDir()
				require.NoError(t, os.Remove(root))
				require.NoError(t, os.Symlink(target, root))
				var err error
				root, _, err = parser.ResolveProviderRoot(parser.AgentQoder, root)
				require.NoError(t, err)
			}
			transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
			require.NoError(t, os.WriteFile(strings.TrimSuffix(transcript, ".jsonl")+"-session.json", []byte(`{"title":"Name from export"}`), 0o644))
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
			case "database rename":
				testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Renamed in the app")
			case "blank row":
				testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "   ")
			case "unreadable file":
				require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o644))
			}
			engine.SyncAll(t.Context(), nil)
			if tc.state == "database rename" {
				assert.Equal(t, new("Renamed in the app"), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
			} else {
				assert.Equal(t, new("Name in the app"), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
			}
			transcript = testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "changed transcript")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			session, err := archive.GetSessionFull(t.Context(), "qoder:"+testjsonl.QoderTitleID)
			require.NoError(t, err)
			assert.Equal(t, new("changed transcript"), session.FirstMessage)
			assert.Equal(t, new(tc.wantName), session.SessionName)
			if tc.state == "unreadable file" {
				require.NoError(t, os.WriteFile(store, original, 0o600))
				testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Recovered rename")
				engine.SyncAll(t.Context(), nil)
				assert.Equal(t, new("Recovered rename"), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
			}
		})
	}
}

func TestQoderMissingRowFollowsSidecarUpdates(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name := "default root"
		if custom {
			name = "custom root"
		}
		t.Run(name, func(t *testing.T) {
			root, store := testjsonl.QoderTitleRoot(t)
			if custom {
				root = t.TempDir()
			}
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
				remove      bool
			}{
				{title: "First", want: "First"},
				{title: "Later", want: "Later"},
				{remove: true, want: "Later"},
				{title: "", want: "Later"},
			} {
				if tc.remove {
					require.NoError(t, os.Remove(sidecar))
				} else {
					require.NoError(t, os.WriteFile(sidecar, []byte(`{"title":"`+tc.title+`"}`), 0o644))
				}
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{sidecar, transcript}))
				assert.Zero(t, engine.LastSyncStats().Failed)
				if custom && tc.title == "" {
					assert.Nil(t, qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
				} else {
					assert.Equal(t, new(tc.want), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
				}
			}
			stats := engine.SyncAll(t.Context(), nil)
			assert.Zero(t, stats.Failed)
			if custom {
				assert.Nil(t, qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
			} else {
				assert.Equal(t, new("Later"), qoderStoredTitle(t, archive, testjsonl.QoderTitleID))
			}
		})
	}
}
