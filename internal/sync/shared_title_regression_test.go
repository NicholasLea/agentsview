package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSharedTitleRegressionUsageOnlySweepIsNoop(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	root := t.TempDir()
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}}, Machine: "devbox", Emitter: emitter, ArchiveContent: config.ArchiveContentUsage})
	t.Cleanup(engine.Close)
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "antigravity:"+id, parser.AgentAntigravity, "", root)
	title := "source title"
	path := writeConversationSummariesDB(t, root, map[string]*string{id: &title})
	for range 2 {
		plan, err := engine.PlanChangedPathsContext(t.Context(), []string{path})
		require.NoError(t, err)
		result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
		require.NoError(t, err)
		assert.Zero(t, result.Stats.TitlesUpdated, "usage policy discards titles, sweep should not repeatedly report a change")
		assert.Empty(t, emitter.take(), "usage-only stable archive must not repeatedly notify")
	}
}

func TestSharedTitleRegressionJSONRecoverySweep(t *testing.T) {
	engine, database := sharedTitleTestEngine(t, nil)
	base := t.TempDir()
	root := filepath.Join(base, ".qoder")
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "qoder:"+id, parser.AgentQoder, "stale", root)
	title := "lower priority"
	path := writeQoderSessionsDB(t, base, "com.qoder.app.stable", map[string]*string{id: &title})
	writePath := filepath.Join(root, "projects", id+"-session.json")
	dbtest.WriteTestFile(t, writePath, []byte(`{broken`))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	requireStoredTitle(t, database, "qoder:"+id, "stale")
	dbtest.WriteTestFile(t, writePath, []byte(`{"title":"recovered JSON title"}`))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	requireStoredTitle(t, database, "qoder:"+id, "recovered JSON title")
}

func TestSharedTitleRegressionDisabledProviderNotRefreshed(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	root := t.TempDir()
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}}, DisabledAgents: []parser.AgentType{parser.AgentAntigravity}, Machine: "devbox", Emitter: emitter})
	t.Cleanup(engine.Close)
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "antigravity:"+id, parser.AgentAntigravity, "disabled old", root)
	title := "must not sync disabled agent"
	path := writeConversationSummariesDB(t, root, map[string]*string{id: &title})
	requireSharedTitleSweep(t, engine, 0)
	// A database event is planned without consulting the provider set, so the
	// refresh itself must skip the disabled provider.
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	requireStoredTitle(t, database, "antigravity:"+id, "disabled old")
	assert.Empty(t, emitter.take())
}

func TestSharedTitleRegressionDifferentMachineNotRefreshed(t *testing.T) {
	engine, database := sharedTitleTestEngine(t, nil)
	base := t.TempDir()
	root := filepath.Join(base, ".qoder")
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	old := "remote old"
	source := filepath.Join(root, "projects", id+".jsonl")
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "qoder:" + id, Agent: string(parser.AgentQoder), Machine: "another-machine",
		FilePath: &source, SessionName: &old,
	}))
	title := "local title"
	path := writeQoderSessionsDB(t, base, "com.qoder.app.stable", map[string]*string{id: &title})
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	requireStoredTitle(t, database, "qoder:"+id, old)
}

func TestSharedTitleRegressionJSONOverridesEqualSQLiteAndNull(t *testing.T) {
	for _, null := range []bool{false, true} {
		name := "equal SQLite"
		if null {
			name = "SQL NULL"
		}
		t.Run(name, func(t *testing.T) {
			emitter := &sharedTitleScopeRecorder{}
			engine, database := sharedTitleTestEngine(t, emitter)
			base := t.TempDir()
			root := filepath.Join(base, ".qoder")
			const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			stale := "stale"
			seedSharedTitleSession(t, database, "qoder:"+id, parser.AgentQoder, stale, root)
			fallback := &stale
			if null {
				fallback = nil
			}
			path := writeQoderSessionsDB(t, base, "com.qoder.app.stable", map[string]*string{id: fallback})
			dbtest.WriteTestFile(t, filepath.Join(root, "projects", id+"-session.json"), []byte(`{"title":"JSON wins"}`))
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
			requireStoredTitle(t, database, "qoder:"+id, "JSON wins")
			assert.Equal(t, []string{"sessions"}, emitter.take())
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
			assert.Empty(t, emitter.take())
		})
	}
}

// TestOrdinaryLocalSyncAppliesTitleOnlyRename covers every ordinary local
// sync entry point. A title-only rename leaves the transcript unchanged, so
// the scan skips it; the sync must still apply the new name under its lock.
func TestOrdinaryLocalSyncAppliesTitleOnlyRename(t *testing.T) {
	noWork := func(bool) error { return nil }
	for name, run := range map[string]func(*Engine) (SyncStats, error){
		"SyncAll": func(e *Engine) (SyncStats, error) { return e.SyncAll(t.Context(), nil), nil },
		"SyncThenRun": func(e *Engine) (SyncStats, error) {
			return e.SyncThenRun(t.Context(), false, nil, noWork)
		},
		"SyncThenRunWithRebuild": func(e *Engine) (SyncStats, error) {
			return e.SyncThenRunWithRebuild(t.Context(), false, nil, nil, nil,
				func(bool, bool) error { return nil })
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			transcript := filepath.Join(root, "brain", id, ".system_generated", "logs", "transcript.jsonl")
			dbtest.WriteTestFile(t, transcript, []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"unchanged body"}`+"\n"))
			first := "first title"
			path := writeConversationSummariesDB(t, root, map[string]*string{id: &first})
			emitter := &sharedTitleScopeRecorder{}
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {root}},
				Machine:   "devbox", Emitter: emitter,
			})
			t.Cleanup(engine.Close)

			stats, err := run(engine)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Synced)
			requireStoredTitle(t, database, "antigravity:"+id, first)
			emitter.take()

			require.NoError(t, os.Remove(path))
			renamed := "renamed title"
			writeConversationSummariesDB(t, root, map[string]*string{id: &renamed})
			stats, err = run(engine)
			require.NoError(t, err)
			assert.Zero(t, stats.Synced, "the unchanged transcript must not be parsed again")
			assert.Equal(t, 1, stats.TitlesUpdated)
			requireStoredTitle(t, database, "antigravity:"+id, renamed)
			assert.NotEmpty(t, emitter.take(), "a title-only pass must notify clients")

			stats, err = run(engine)
			require.NoError(t, err)
			assert.Zero(t, stats.TitlesUpdated)
		})
	}
}

// TestSharedTitleRefreshComparesSanitizedTitle keeps control characters that
// every write strips from turning a steady state into a rewrite, a
// local_modified_at bump, and a notification on every sweep.
func TestSharedTitleRefreshComparesSanitizedTitle(t *testing.T) {
	emitter := &sharedTitleScopeRecorder{}
	root := t.TempDir()
	engine, database := sharedTitleTestEngineForDirs(t, emitter,
		map[parser.AgentType][]string{parser.AgentAntigravity: {root}})
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	seedSharedTitleSession(t, database, "antigravity:"+id, parser.AgentAntigravity, "old", root)
	title := "Fix\x00 the\x1b parser"
	writeConversationSummariesDB(t, root, map[string]*string{id: &title})

	requireSharedTitleSweep(t, engine, 1)
	requireStoredTitle(t, database, "antigravity:"+id, "Fix the parser")
	assert.Equal(t, []string{"sessions"}, emitter.take())

	requireSharedTitleSweep(t, engine, 0)
	assert.Empty(t, emitter.take(), "an unchanged sanitized title must not notify again")
}

// TestQoderTitleRefreshHonorsRelabeledRoot covers a native Qoder root that is
// relabeled to another machine after its sessions were imported. The rows
// keep their local attribution, but the root's current owner, resolved from
// the most specific configured label, decides whether this machine's
// application database may title them.
func TestQoderTitleRefreshHonorsRelabeledRoot(t *testing.T) {
	for name, labels := range map[string]func(base string) map[string]string{
		"root relabeled": func(base string) map[string]string {
			return map[string]string{filepath.Join(base, ".qoder", "projects"): "otherhost"}
		},
		"nested label overrides ancestor": func(base string) map[string]string {
			return map[string]string{
				filepath.Join(base, ".qoder"):                     "devbox",
				filepath.Join(base, ".qoder", "projects", "work"): "otherhost",
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			title := "local application title"
			writeQoderSessionsDB(t, base, "com.qoder.app.stable", map[string]*string{id: &title})
			root := filepath.Join(base, ".qoder", "projects")
			emitter := &sharedTitleScopeRecorder{}
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs:      map[parser.AgentType][]string{parser.AgentQoder: {root}},
				SourceMachines: map[parser.AgentType]map[string]string{parser.AgentQoder: labels(base)},
				Machine:        "devbox", Emitter: emitter,
			})
			t.Cleanup(engine.Close)
			source := filepath.Join(root, "work", id+".jsonl")
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "qoder:" + id, Agent: string(parser.AgentQoder), Machine: "devbox",
				FilePath: &source, SessionName: strPtr("imported title"),
			}))

			requireSharedTitleSweep(t, engine, 0)
			requireStoredTitle(t, database, "qoder:"+id, "imported title")
			assert.Empty(t, emitter.take())
		})
	}
}

// TestSharedTitleRefreshCountSurvivesUnchangedFilesInBatch covers a batch
// that carries a title database event together with an unchanged transcript.
// The body pass skips the transcript, so the title count is the only change
// and must survive the body stats or clients are never notified.
func TestSharedTitleRefreshCountSurvivesUnchangedFilesInBatch(t *testing.T) {
	for _, entry := range []string{"changed paths", "planned paths"} {
		t.Run(entry, func(t *testing.T) {
			root := t.TempDir()
			const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			transcript := filepath.Join(root, "brain", id, ".system_generated", "logs", "transcript.jsonl")
			dbtest.WriteTestFile(t, transcript, []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"unchanged body"}`+"\n"))
			first := "first title"
			path := writeConversationSummariesDB(t, root, map[string]*string{id: &first})
			emitter := &sharedTitleScopeRecorder{}
			engine, database := sharedTitleTestEngineForDirs(t, emitter,
				map[parser.AgentType][]string{parser.AgentAntigravity: {root}})
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			requireStoredTitle(t, database, "antigravity:"+id, first)
			emitter.take()

			require.NoError(t, os.Remove(path))
			renamed := "renamed title"
			writeConversationSummariesDB(t, root, map[string]*string{id: &renamed})
			batch := []string{path, transcript}
			if entry == "changed paths" {
				require.NoError(t, engine.SyncPathsContext(t.Context(), batch))
			} else {
				plan, err := engine.PlanChangedPathsContext(t.Context(), batch)
				require.NoError(t, err)
				require.NotEmpty(t, plan.Files)
				result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
				require.NoError(t, err)
				assert.Zero(t, result.Stats.Synced)
				assert.Equal(t, 1, result.Stats.TitlesUpdated)
			}
			requireStoredTitle(t, database, "antigravity:"+id, renamed)
			assert.Equal(t, []string{"sessions"}, emitter.take())
		})
	}
}

// TestQoderTitleRefreshThroughSymlinkedNativeRoot covers native Qoder trees
// linked outside the home directory: the client directory, its projects
// directory, or the home directory itself. Configured roots are resolved to
// their physical destination, so the refresh must still bind them to this
// machine's application database, including a database event reported
// through the physical path of a linked home.
func TestQoderTitleRefreshThroughSymlinkedNativeRoot(t *testing.T) {
	const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	for _, position := range []string{"client directory", "projects directory", "home directory"} {
		t.Run(position, func(t *testing.T) {
			physical := filepath.Join(t.TempDir(), "physical")
			require.NoError(t, os.MkdirAll(physical, 0o755))
			home := t.TempDir()
			link := filepath.Join(home, ".qoder")
			switch position {
			case "projects directory":
				require.NoError(t, os.MkdirAll(link, 0o755))
				link = filepath.Join(link, "projects")
			case "home directory":
				home = filepath.Join(t.TempDir(), "home")
				link = home
			}
			if err := os.Symlink(physical, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			title := "linked native title"
			dbPath := writeQoderSessionsDB(t, home, "com.qoder.app.stable", map[string]*string{id: &title})
			root, _, err := parser.ResolveProviderRoot(parser.AgentQoder, filepath.Join(home, ".qoder", "projects"))
			require.NoError(t, err)
			require.NotEqual(t, filepath.Join(home, ".qoder", "projects"), root)
			emitter := &sharedTitleScopeRecorder{}
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentQoder: {root}},
				Machine:   "devbox", Emitter: emitter,
			})
			t.Cleanup(engine.Close)
			source := filepath.Join(root, "work", id+".jsonl")
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "qoder:" + id, Agent: string(parser.AgentQoder), Machine: "devbox",
				FilePath: &source, SessionName: strPtr("old"),
			}))

			requireSharedTitleSweep(t, engine, 1)
			requireStoredTitle(t, database, "qoder:"+id, title)
			assert.Equal(t, []string{"sessions"}, emitter.take())

			renamed := "renamed through event"
			require.NoError(t, os.Remove(dbPath))
			writeQoderSessionsDB(t, home, "com.qoder.app.stable", map[string]*string{id: &renamed})
			event, err := filepath.EvalSymlinks(dbPath)
			require.NoError(t, err)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{event}))
			requireStoredTitle(t, database, "qoder:"+id, renamed)
		})
	}
}
