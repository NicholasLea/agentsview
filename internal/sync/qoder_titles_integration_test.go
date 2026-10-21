package sync_test

import (
	"os"
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
	}{
		{"database rename", "Renamed in the app"},
		{"blank row", "Name in the app"},
		{"unreadable file", "Name in the app"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			root, store := testjsonl.QoderTitleRoot(t)
			transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
			require.NoError(t, os.WriteFile(strings.TrimSuffix(transcript, ".jsonl")+"-session.json", []byte(`{"title":"Name from export"}`), 0o644))
			testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Name in the app")
			archive := dbtest.OpenTestDB(t)
			engine := newNativeTitleEngine(t, archive, parser.AgentQoder, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
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
				title                   string
				wantDefault, wantCustom *string
				remove                  bool
			}{
				{title: "First", wantDefault: new("First"), wantCustom: new("First")},
				{title: "Later", wantDefault: new("Later"), wantCustom: new("Later")},
				{remove: true, wantDefault: new("Later"), wantCustom: nil},
				{title: "", wantDefault: new("Later"), wantCustom: nil},
			} {
				if tc.remove {
					require.NoError(t, os.Remove(sidecar))
				} else {
					require.NoError(t, os.WriteFile(sidecar, []byte(`{"title":"`+tc.title+`"}`), 0o644))
				}
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{sidecar, transcript}))
				assert.Zero(t, engine.LastSyncStats().Failed)
				want := tc.wantDefault
				if custom {
					want = tc.wantCustom
				}
				assert.Equal(t, want, nativeStoredTitle(t, archive, "qoder:"+testjsonl.QoderTitleID))
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
