package sync_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestQoderTitleFallbackOnReparse(t *testing.T) {
	for _, tc := range []struct {
		state, wantName string
		symlink         string
	}{
		{"database rename", "Renamed in the app", ""},
		{"database rename", "Renamed in the app", "default root"},
		{"database rename", "Renamed in the app", "file"},
		{"database rename", "Renamed in the app", "project directory"},
		{"blank row", "Name in the app", ""},
		{"unreadable file", "Name in the app", ""},
	} {
		name := tc.state
		if tc.symlink != "" {
			name += " symlinked " + tc.symlink
		}
		t.Run(name, func(t *testing.T) {
			root, store := testjsonl.QoderTitleRoot(t)
			if tc.symlink == "default root" {
				target := t.TempDir()
				require.NoError(t, os.Remove(root))
				require.NoError(t, os.Symlink(target, root))
				var err error
				root, _, err = parser.ResolveProviderRoot(parser.AgentQoder, root)
				require.NoError(t, err)
			}
			transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
			switch tc.symlink {
			case "file", "project directory":
				link := transcript
				if tc.symlink == "project directory" {
					link = filepath.Dir(transcript)
				}
				target := filepath.Join(t.TempDir(), filepath.Base(link))
				require.NoError(t, os.Rename(link, target))
				require.NoError(t, os.Symlink(target, link))
			}
			require.NoError(t, os.WriteFile(strings.TrimSuffix(transcript, ".jsonl")+"-session.json", []byte(`{"title":"Name from export"}`), 0o644))
			testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Name in the app")
			parsed, err := parser.ParseQoderSession(transcript, "sample-project", "host-a")
			require.NoError(t, err)
			require.Len(t, parsed, 1)
			assert.Equal(t, "Name in the app", parsed[0].Session.SessionName)
			assert.True(t, parsed[0].Session.SessionNamePresent)
			archive := dbtest.OpenTestDB(t)
			engine := newNativeTitleEngine(t, archive, parser.AgentQoder, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			require.Equal(t, new("Name in the app"), nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
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
				assert.Equal(t, new("Renamed in the app"), nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
			} else {
				assert.Equal(t, new("Name in the app"), nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
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
				assert.Equal(t, new("Recovered rename"), nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
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
			testjsonl.SetQoderTitle(t, store, "other-session", "Other title")
			transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
			sidecar := strings.TrimSuffix(transcript, ".jsonl") + "-session.json"
			archive := dbtest.OpenTestDB(t)
			engine := newNativeTitleEngine(t, archive, parser.AgentQoder, root)
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
					assert.Nil(t, nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
				} else {
					assert.Equal(t, new(tc.want), nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
				}
			}
			stats := engine.SyncAll(t.Context(), nil)
			assert.Zero(t, stats.Failed)
			if custom {
				assert.Nil(t, nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
			} else {
				assert.Equal(t, new("Later"), nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
			}
		})
	}
}
