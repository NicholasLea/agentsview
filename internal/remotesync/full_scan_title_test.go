package remotesync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

// writeAntigravityBrain creates a minimal Antigravity brain transcript with
// one conversation and returns the transcript path. root should be the
// Antigravity IDE root (the parent of "brain").
func writeAntigravityBrain(t *testing.T, root, convID, firstMessage string) string {
	t.Helper()
	dir := filepath.Join(root, "brain", convID, ".system_generated", "logs")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "transcript.jsonl")
	content := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-05-20T10:00:00Z","content":"` + firstMessage + `"}
{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-20T10:00:01Z","content":"response"}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// writeConversationSummariesDB creates an Antigravity conversation_summaries.db
// with the given title map and returns the database path.
func writeConversationSummariesDB(t *testing.T, root string, titles map[string]string) string {
	t.Helper()
	path := parser.AntigravityTitleDatabasePath(root)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`CREATE TABLE IF NOT EXISTS conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT)`)
	require.NoError(t, err)
	for id, title := range titles {
		_, err = conn.Exec(`INSERT OR REPLACE INTO conversation_summaries VALUES (?, ?)`, id, title)
		require.NoError(t, err)
	}
	return path
}

// updateConversationSummariesDB updates an existing conversation_summaries.db.
func updateConversationSummariesDB(t *testing.T, dbPath string, titles map[string]string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	for id, title := range titles {
		_, err = conn.Exec(`UPDATE conversation_summaries SET title = ? WHERE conversation_id = ?`, title, id)
		require.NoError(t, err)
	}
}

// TestLegacyImportExtractedRefreshesTitles ensures the legacy ImportExtracted
// entry point reconciles shared titles after scanning transcripts, even when
// transcripts are cached and unchanged.
func TestLegacyImportExtractedRefreshesTitles(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	host := "test-host"

	// Set up Antigravity root with transcript and title database
	antigravityRoot := filepath.Join(root, "antigravity")
	writeAntigravityBrain(t, antigravityRoot, "conv-001", "Initial message")
	titleDB := writeConversationSummariesDB(t, antigravityRoot, map[string]string{
		"conv-001": "Initial title",
	})

	targets := TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentAntigravity: {antigravityRoot},
		},
	}

	// First import: process transcript and title
	importer := Importer{
		DB:      database,
		Host:    host,
		Targets: targets,
		Root:    root,
		Full:    false,
	}
	stats1, err := importer.ImportExtracted(t.Context(), targets, root)
	require.NoError(t, err)
	assert.Equal(t, 1, stats1.SessionsSynced)
	assert.Equal(t, 1, stats1.TitlesUpdated)

	// Verify stored title
	expectedID := host + "~antigravity:conv-001"
	stored, err := database.GetSession(t.Context(), expectedID)
	require.NoError(t, err)
	assert.Equal(t, "Initial title", *stored.SessionName)

	// Update only the title database (transcript unchanged)
	updateConversationSummariesDB(t, titleDB, map[string]string{
		"conv-001": "Renamed title",
	})

	// Second import: transcript cached, but title should refresh
	stats2, err := importer.ImportExtracted(t.Context(), targets, root)
	require.NoError(t, err)
	assert.Zero(t, stats2.SessionsSynced, "cached transcript should not reparse")
	assert.Equal(t, 1, stats2.TitlesUpdated, "title-only change must refresh")

	// Verify title was updated
	updated, err := database.GetSession(t.Context(), expectedID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed title", *updated.SessionName)
}

// TestFullScanWithoutForceParseRefreshesTitles ensures a non-forced full scan
// (pending.full without ForceParse) still reconciles titles when transcripts
// are cached.
func TestFullScanWithoutForceParseRefreshesTitles(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	host := "test-host"

	// Set up Antigravity root
	antigravityRoot := filepath.Join(root, "antigravity")
	brainPath := writeAntigravityBrain(t, antigravityRoot, "conv-002", "Original")
	titleDB := writeConversationSummariesDB(t, antigravityRoot, map[string]string{
		"conv-002": "Original",
	})

	targets := TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentAntigravity: {antigravityRoot},
		},
	}

	// First import with delta to establish cache
	importer := Importer{
		DB:      database,
		Host:    host,
		Targets: targets,
		Root:    root,
	}
	journal := MirrorChangeJournal{
		Entries: []MirrorChangeEntry{
			{Path: toRemotePath(root, brainPath), InvalidateCache: true},
		},
	}
	prepared, err := importer.PreparePending(t.Context(), DeltaImportRequest{
		Journal: journal,
	})
	require.NoError(t, err)
	stats1, err := prepared.Execute(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, stats1.SessionsSynced)

	// Update only title
	updateConversationSummariesDB(t, titleDB, map[string]string{
		"conv-002": "Updated title",
	})

	// Trigger full scan without ForceParse (e.g., bootstrap)
	journal2 := MirrorChangeJournal{
		FullImport:       true,
		FullImportReason: FullImportBootstrap,
	}
	prepared2, err := importer.PreparePending(t.Context(), DeltaImportRequest{
		Journal: journal2,
	})
	require.NoError(t, err)
	require.True(t, prepared2.full)
	require.False(t, prepared2.forceParse)

	stats2, err := prepared2.Execute(t.Context())
	require.NoError(t, err)
	assert.Zero(t, stats2.SessionsSynced, "cached transcript should not reparse")
	assert.Equal(t, 1, stats2.TitlesUpdated, "full scan must refresh titles")

	// Verify title was updated
	expectedID := host + "~antigravity:conv-002"
	updated, err := database.GetSession(t.Context(), expectedID)
	require.NoError(t, err)
	assert.Equal(t, "Updated title", *updated.SessionName)
}

// TestFullScanTitleClearWorks ensures full scan can clear titles (blank value).
func TestFullScanTitleClearWorks(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	host := "test-host"

	antigravityRoot := filepath.Join(root, "antigravity")
	writeAntigravityBrain(t, antigravityRoot, "conv-003", "Will be cleared")
	titleDB := writeConversationSummariesDB(t, antigravityRoot, map[string]string{
		"conv-003": "Will be cleared",
	})

	targets := TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentAntigravity: {antigravityRoot},
		},
	}

	importer := Importer{
		DB:      database,
		Host:    host,
		Targets: targets,
		Root:    root,
	}

	// First import
	stats1, err := importer.ImportExtracted(t.Context(), targets, root)
	require.NoError(t, err)
	assert.Equal(t, 1, stats1.SessionsSynced)

	expectedID := host + "~antigravity:conv-003"
	stored, err := database.GetSession(t.Context(), expectedID)
	require.NoError(t, err)
	assert.Equal(t, "Will be cleared", *stored.SessionName)

	// Clear the title (empty string)
	updateConversationSummariesDB(t, titleDB, map[string]string{
		"conv-003": "",
	})

	// Second import should clear
	stats2, err := importer.ImportExtracted(t.Context(), targets, root)
	require.NoError(t, err)
	assert.Equal(t, 1, stats2.TitlesUpdated)

	cleared, err := database.GetSession(t.Context(), expectedID)
	require.NoError(t, err)
	assert.Nil(t, cleared.SessionName, "explicit clear must set session_name to NULL")
}

func toRemotePath(root, localPath string) string {
	rel, err := filepath.Rel(root, localPath)
	if err != nil {
		return localPath
	}
	return filepath.ToSlash(rel)
}
