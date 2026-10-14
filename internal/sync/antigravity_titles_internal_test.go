package sync

import (
	"bytes"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestAntigravityMatchingMarkersExpireOrRefreshOnRequest(t *testing.T) {
	for _, mode := range []string{"interval", "watcher", "plan", "full"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
			priorNow := openCodeContainerDigestVerifyNow
			openCodeContainerDigestVerifyNow = func() time.Time { return now }
			t.Cleanup(func() { openCodeContainerDigestVerifyNow = priorNow })
			root := t.TempDir()
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			store := dbtest.SetAntigravityTitle(t, root, "First")
			conn, err := sql.Open("sqlite3", store)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, conn.Close()) })
			_, err = conn.Exec("PRAGMA journal_mode=WAL; UPDATE conversation_summaries SET title = 'First'")
			require.NoError(t, err)
			checkpoint := func() {
				_, err := conn.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
				require.NoError(t, err)
				for _, path := range []string{store, store + "-wal"} {
					require.NoError(t, os.Chtimes(path, now, now))
				}
			}
			checkpoint()
			archive := openTestDB(t)
			engine := NewEngine(t.Context(), archive, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}}, Machine: "host-a"})
			t.Cleanup(engine.Close)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			before, ok := parser.StatSQLiteContainerState(store)
			require.True(t, ok)
			_, err = conn.Exec("UPDATE conversation_summaries SET title = 'Later'")
			require.NoError(t, err)
			checkpoint()
			after, ok := parser.StatSQLiteContainerState(store)
			require.True(t, ok)
			require.Equal(t, before, after, "a checkpointed rename can preserve every marker")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{filepath.Join(root, "unrelated")}))
			session, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
			require.NoError(t, err)
			require.Equal(t, new("First"), session.SessionName)
			switch mode {
			case "interval":
				now = now.Add(openCodeContainerDigestVerifyInterval)
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{filepath.Join(root, "unrelated")}))
			case "watcher":
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store + "-wal"}))
			case "plan":
				plan, err := engine.PlanChangedPathsContext(t.Context(), []string{store})
				require.NoError(t, err)
				_, err = engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
				require.NoError(t, err)
			case "full":
				engine.SyncAll(t.Context(), nil)
			}
			session, err = archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
			require.NoError(t, err)
			assert.Equal(t, new("Later"), session.SessionName)
			assert.Equal(t, 1, engine.LastSyncStats().TitlesUpdated)
		})
	}
}

func TestAntigravityFailedReadRetriesAfterInterval(t *testing.T) {
	now := time.Now()
	priorNow := openCodeContainerDigestVerifyNow
	openCodeContainerDigestVerifyNow = func() time.Time { return now }
	t.Cleanup(func() { openCodeContainerDigestVerifyNow = priorNow })
	var output bytes.Buffer
	priorOutput := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(priorOutput) })
	root := t.TempDir()
	store := filepath.Join(root, parser.AntigravitySummariesDBName)
	conn, err := sql.Open("sqlite3", store)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.Exec("CREATE TABLE other (title TEXT)")
	require.NoError(t, err)
	engine := NewEngine(t.Context(), openTestDB(t), EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}}})
	t.Cleanup(engine.Close)
	pass := func() {
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{filepath.Join(root, "unrelated")}))
	}
	pass()
	require.Contains(t, output.String(), "antigravity titles")
	output.Reset()
	pass()
	assert.NotContains(t, output.String(), "antigravity titles")
	_, err = conn.Exec("CREATE TABLE other_changed (title TEXT)")
	require.NoError(t, err)
	pass()
	require.Contains(t, output.String(), "antigravity titles")
	output.Reset()
	pass()
	assert.NotContains(t, output.String(), "antigravity titles")
	now = now.Add(openCodeContainerDigestVerifyInterval)
	pass()
	assert.Contains(t, output.String(), "antigravity titles")
}
