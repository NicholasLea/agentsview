package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// The Qoder title database sits in an Electron user-data directory that churns
// constantly (cookies, logs, journals). Those events must not resolve to a
// transcript, and a batch holding only them must not rename anything.
func TestQoderTitleDirectoryNoisePlansNoSources(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "First")
	archive := openTestDB(t)
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentQoder: {root}},
		Machine:   "host-a",
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	noise := filepath.Join(filepath.Dir(store), "Cookies")
	require.NoError(t, os.WriteFile(noise, []byte("churn"), 0o600))
	files, err := engine.classifyPaths(t.Context(), []string{noise})
	require.NoError(t, err)
	assert.Empty(t, files, "user-data churn must not resolve to a transcript")

	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{noise}))
	assert.Equal(t, new("First"), qoderSessionName(t, archive, testjsonl.QoderTitleID))

	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Renamed")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Renamed"), qoderSessionName(t, archive, testjsonl.QoderTitleID))
}

// TestQoderFramelessWALEventsDoNotRereadStore pins that the sibling files our
// own read-only connection leaves behind do not schedule another read. Left
// unguarded, each refresh would emit the event asking for the next one.
func TestQoderFramelessWALEventsDoNotRereadStore(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	transcript := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "hello")
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "First")
	archive := openTestDB(t)
	engine := NewEngine(t.Context(), archive, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentQoder: {root}},
		Machine:   "host-a",
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	spy := &antigravityTitleQuerySpy{Store: archive, reader: archive}
	engine.archiveStore = spy
	engine.deferredNativeTitles = make(map[string]*string)

	for _, sibling := range []string{"-shm", "-wal"} {
		require.NoError(t, os.WriteFile(store+sibling, nil, 0o600))
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store + sibling}))
		assert.Empty(t, spy.queries, "a reader-created %s must not reread titles", sibling)
	}
}

func qoderSessionName(t *testing.T, archive *db.DB, id string) *string {
	t.Helper()
	session, err := archive.GetSessionFull(t.Context(), "qoder:"+id)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session.SessionName
}
