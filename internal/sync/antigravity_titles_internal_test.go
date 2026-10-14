package sync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

type antigravityTitleQuerySpy struct {
	db.Store
	reader  sessionWriteIdentityReader
	queries [][]string
}

func (s *antigravityTitleQuerySpy) ListSessionWriteIdentitiesByID(ctx context.Context, ids []string) (map[string]db.SessionWriteIdentity, error) {
	s.queries = append(s.queries, append([]string(nil), ids...))
	return s.reader.ListSessionWriteIdentitiesByID(ctx, ids)
}

func TestAntigravityUnrelatedChangedPathSkipsUnchangedStoreQueries(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	store := dbtest.SetAntigravityTitle(t, root, "First")
	archive := openTestDB(t)
	engine := NewEngine(t.Context(), archive, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}}, Machine: "host-a"})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	spy := &antigravityTitleQuerySpy{Store: archive, reader: archive}
	engine.archiveStore = spy
	engine.deferredAntigravityTitles = make(map[string]*string)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	require.Equal(t, [][]string{{"antigravity:" + dbtest.AntigravityTitleID}}, spy.queries)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{filepath.Join(root, "unrelated")}))
	assert.Len(t, spy.queries, 1)
	plan, err := engine.PlanChangedPathsContext(t.Context(), []string{filepath.Join(root, "unrelated")})
	require.NoError(t, err)
	_, err = engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
	require.NoError(t, err)
	assert.Len(t, spy.queries, 1)
}

func TestAntigravityTitleFailureDoesNotRetryOtherAgents(t *testing.T) {
	for _, mode := range []string{"watcher", "plan", "recovery", "grouped"} {
		t.Run(mode, func(t *testing.T) {
			root, claudeRoot := t.TempDir(), t.TempDir()
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			store := dbtest.SetAntigravityTitle(t, root, "First")
			claudePath := writeClaudeProjectSession(t, claudeRoot, "project", "missing")
			archive := openTestDB(t)
			engine := NewEngine(t.Context(), archive, EngineConfig{AgentDirs: map[parser.AgentType][]string{
				parser.AgentAntigravity: {root}, parser.AgentClaude: {claudeRoot},
			}, Machine: "host-a"})
			t.Cleanup(engine.Close)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, claudePath}))
			require.NoError(t, os.Remove(claudePath))
			if mode == "plan" {
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{claudePath}))
			}
			beforeSync, beforeStats := engine.LastSync(), engine.LastSyncStats()
			raw, err := sql.Open("sqlite3", archive.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			_, err = raw.Exec(`CREATE TRIGGER fail_title_write BEFORE UPDATE OF session_name ON sessions BEGIN SELECT RAISE(FAIL, 'injected title write failure'); END`)
			require.NoError(t, err)
			dbtest.SetAntigravityTitle(t, root, "Later")
			spy := &antigravityTitleQuerySpy{Store: archive, reader: archive}
			engine.archiveStore = spy
			engine.deferredAntigravityTitles = make(map[string]*string)
			switch mode {
			case "watcher":
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{claudePath}))
			case "plan":
				plan, err := engine.PlanChangedPathsContext(t.Context(), []string{store})
				require.NoError(t, err)
				result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
				require.NoError(t, err)
				assert.False(t, result.Stats.ProcessingComplete())
			case "recovery":
				stats, _, err := engine.ReconcileWatchRootsWithStats(t.Context(), []string{claudeRoot}, false, nil)
				require.NoError(t, err)
				assert.Equal(t, 1, stats.Failed)
			case "grouped":
				require.NoError(t, engine.ReconcileProviderRootsGrouped(t.Context(), []ProviderRootsGroup{
					{Agent: parser.AgentClaude, Roots: []string{claudeRoot}},
					{Agent: parser.AgentAntigravity, Roots: []string{root}},
				}))
			}
			assert.Len(t, spy.queries, 1, "the grouped pass attempts each title store once")
			assert.Equal(t, 1, engine.LastSyncStats().Failed)
			assert.Empty(t, engine.SnapshotAntigravityTitleStates())
			session, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
			require.NoError(t, err)
			assert.Equal(t, new("First"), session.SessionName)
			if mode == "plan" {
				assert.Equal(t, beforeSync, engine.LastSync())
			}
			var missing sql.NullString
			require.NoError(t, archive.Reader().QueryRow(t.Context(), "SELECT source_missing_at FROM sessions WHERE id = 'missing'").Scan(&missing))
			assert.True(t, missing.Valid, "title failures preserve other agents' deletion proof")
			_, err = raw.Exec("DROP TRIGGER fail_title_write")
			require.NoError(t, err)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{filepath.Join(root, "unrelated")}))
			session, err = archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
			require.NoError(t, err)
			assert.Equal(t, new("Later"), session.SessionName)
			assert.Equal(t, beforeStats.TitlesUpdated+1, engine.LastSyncStats().TitlesUpdated)
			assert.True(t, engine.LastSync().After(beforeSync))
		})
	}
}

func TestAntigravityMatchingMarkersExpireOrRefreshOnRequest(t *testing.T) {
	for _, mode := range []string{"interval", "watcher", "plan", "full"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now()
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
					time.Sleep(parser.SQLiteContainerDigestVerifyInterval)
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
		})
	}
}
