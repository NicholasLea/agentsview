package remotesync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

// TestAntigravityTitleOnlyRemoteFullImport covers the full-scan entry points.
// A title-only rename leaves the transcript unchanged, so the scan skips it
// through the persisted cache; the import must still apply the summaries
// database, report the change, and fail without losing the stored name when
// the database cannot be read. A forced parse applies the title itself, so its
// reconcile pass must not count the same change a second time.
func TestAntigravityTitleOnlyRemoteFullImport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
		forced bool
	}{
		{name: "legacy import", legacy: true},
		{name: "forced legacy import", legacy: true, forced: true},
		{name: "full delta import"},
		{name: "forced full delta import", forced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			extracted := t.TempDir()
			const remoteRoot = "/srv/antigravity"
			const bare = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			const id = "remote~antigravity:" + bare
			root := remappedRemotePath(extracted, remoteRoot)
			transcript := filepath.Join(root, "brain", bare, ".system_generated", "logs", "transcript.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(transcript), 0o755))
			require.NoError(t, os.WriteFile(transcript, []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-05-20T22:10:00Z","content":"remote title fixture"}
{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-20T22:10:01Z","content":"answer"}
`), 0o600))
			titlePath := filepath.Join(root, "conversation_summaries.db")
			writeTitle := func(title string) {
				t.Helper()
				require.NoError(t, os.RemoveAll(titlePath))
				conn, err := sql.Open("sqlite3", titlePath)
				require.NoError(t, err)
				defer conn.Close()
				_, err = conn.ExecContext(t.Context(), `CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT); INSERT INTO conversation_summaries VALUES (?, ?)`, bare, title)
				require.NoError(t, err)
			}
			writeTitle("old")
			targets := TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentAntigravity: {remoteRoot}}}
			// RequireComplete matches the HTTP legacy import; delta imports
			// ignore it.
			importer := Importer{Host: "remote", DB: database, Root: extracted, Targets: targets, RequireComplete: true}

			// The first import parses the transcript, takes the title from the
			// parse, and persists the skip cache the later imports rely on.
			stats, err := importer.ImportExtracted(t.Context(), targets, extracted)
			require.NoError(t, err)
			require.Equal(t, 1, stats.SessionsSynced)
			assert.Zero(t, stats.TitlesUpdated, "a title applied by the parse is not counted again")
			requireRemoteTitle(t, database, id, "old")
			before, err := database.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			require.NotEmpty(t, before)
			display := "user override"
			require.NoError(t, database.RenameSession(t.Context(), id, &display))

			run := func() (SyncStats, error) {
				t.Helper()
				if tc.legacy {
					im := importer
					im.Full = tc.forced
					return im.ImportExtracted(t.Context(), targets, extracted)
				}
				// An overflowed journal replaces its path entries, including the
				// summaries database, with a full scan.
				pending, err := importer.PreparePending(t.Context(), DeltaImportRequest{
					Journal: MirrorChangeJournal{
						Version: mirrorJournalVersion, FullImport: true,
						FullImportReason: FullImportJournalOverflow,
					},
					ForceParse: tc.forced,
				})
				require.NoError(t, err)
				require.True(t, pending.full)
				return pending.Execute(t.Context())
			}
			requireApplied := func(want string) {
				t.Helper()
				stats, err := run()
				require.NoError(t, err)
				if !tc.legacy {
					assert.Equal(t, JournalRetired, stats.JournalOutcome)
				}
				if tc.forced {
					assert.Equal(t, 1, stats.SessionsSynced)
					assert.Zero(t, stats.TitlesUpdated, "a forced parse already applied the title")
				} else {
					assert.Zero(t, stats.SessionsSynced, "the cached transcript must not be reparsed")
					assert.Equal(t, 1, stats.TitlesUpdated)
				}
				requireRemoteTitle(t, database, id, want)
			}

			writeTitle("renamed")
			requireApplied("renamed")

			// An unreadable store keeps the stored name and fails the import
			// so the change is retried; a delta import keeps its journal.
			require.NoError(t, os.WriteFile(titlePath, []byte("not a database"), 0o600))
			stats, err = run()
			require.Error(t, err)
			assert.Zero(t, stats.TitlesUpdated)
			if !tc.legacy {
				assert.Equal(t, JournalProcessingFailures, stats.JournalOutcome)
			}
			requireRemoteTitle(t, database, id, "renamed")

			writeTitle("")
			requireApplied("")

			if !tc.forced {
				stats, err = run()
				require.NoError(t, err)
				assert.Zero(t, stats.TitlesUpdated, "a steady state reports no change")
			}

			session, err := database.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, session.DisplayName)
			assert.Equal(t, display, *session.DisplayName)
			after, err := database.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			if tc.forced {
				require.Len(t, after, len(before))
				for i := range before {
					assert.Equal(t, before[i].Content, after[i].Content)
				}
			} else {
				assert.Equal(t, before, after, "title-only imports must preserve messages")
			}
		})
	}
}

func requireRemoteTitle(t *testing.T, database *db.DB, id, want string) {
	t.Helper()
	name, found, err := database.GetSessionName(t.Context(), id)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, want, name)
}
