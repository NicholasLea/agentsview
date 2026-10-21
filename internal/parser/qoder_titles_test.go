package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func readTitles(t *testing.T, store string) map[string]string {
	t.Helper()
	titles, _, err := QoderStoreTitles(t.Context(), store)
	require.NoError(t, err)
	return titles
}

// closeWALStore leaves a WAL-mode database with no sibling files, the state a
// machine whose Qoder app is not running presents.
func closeWALStore(t *testing.T, store string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", store)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), "PRAGMA journal_mode=WAL")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	for _, sibling := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(store + sibling); err == nil {
			require.NoError(t, os.Remove(store+sibling))
		}
	}
}

func TestQoderStoreTitles(t *testing.T) {
	_, store := testjsonl.QoderTitleRoot(t)
	testjsonl.SetQoderTitle(t, store, "plain-id", "  Renamed  ")
	testjsonl.SetQoderTitle(t, store, "blank-id", "   ")
	testjsonl.SetQoderTitle(t, store, "empty-id", "")
	titles := readTitles(t, store)
	assert.Equal(t, "Renamed", titles["plain-id"], "outer whitespace is trimmed")
	// Asserted through the lookup result rather than the value alone: a missing
	// key would satisfy an emptiness check on its own.
	blank, blankRow := titles["blank-id"]
	assert.True(t, blankRow, "a whitespace-only row stays in the map for the parse to see")
	assert.Empty(t, blank)
	empty, emptyRow := titles["empty-id"]
	assert.True(t, emptyRow)
	assert.Empty(t, empty)
}

// A closed Qoder database can retain WAL mode without sibling files.
func TestQoderStoreTitlesWithoutWALSiblings(t *testing.T) {
	_, store := testjsonl.QoderTitleRoot(t)
	testjsonl.SetQoderTitle(t, store, "id", "First")
	closeWALStore(t, store)
	assert.Equal(t, "First", readTitles(t, store)["id"])
}

func TestQoderStoreTitlesErrors(t *testing.T) {
	t.Run("missing table", func(t *testing.T) {
		_, store := testjsonl.QoderTitleRoot(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(store), 0o755))
		conn, err := sql.Open("sqlite3", store)
		require.NoError(t, err)
		_, err = conn.ExecContext(t.Context(), "CREATE TABLE other (id TEXT)")
		require.NoError(t, err)
		require.NoError(t, conn.Close())
		_, _, err = QoderStoreTitles(t.Context(), store)
		assert.Error(t, err)
	})
	t.Run("missing column", func(t *testing.T) {
		_, store := testjsonl.QoderTitleRoot(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(store), 0o755))
		conn, err := sql.Open("sqlite3", store)
		require.NoError(t, err)
		_, err = conn.ExecContext(t.Context(),
			"CREATE TABLE chat_sessions (session_id TEXT, name TEXT)")
		require.NoError(t, err)
		require.NoError(t, conn.Close())
		_, _, err = QoderStoreTitles(t.Context(), store)
		assert.Error(t, err)
	})
	t.Run("not a database", func(t *testing.T) {
		_, store := testjsonl.QoderTitleRoot(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(store), 0o755))
		require.NoError(t, os.WriteFile(store, []byte("not sqlite"), 0o600))
		_, _, err := QoderStoreTitles(t.Context(), store)
		assert.Error(t, err)
	})
	t.Run("absent store is empty", func(t *testing.T) {
		_, store := testjsonl.QoderTitleRoot(t)
		titles, _, err := QoderStoreTitles(t.Context(), store)
		require.NoError(t, err)
		assert.Empty(t, titles)
	})
}

func TestQoderTitleDatabase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, tc := range []struct {
		name string
		root string
		want string
	}{
		{
			name: "international",
			root: filepath.Join(home, ".qoder", "projects"),
			want: filepath.Join(home, "Library", "Application Support",
				"com.qoder.app.stable", QoderTitleDBName),
		},
		{
			name: "cn",
			root: filepath.Join(home, ".qoder-cn", "projects"),
			want: filepath.Join(home, "Library", "Application Support",
				"com.qodercn.app.stable", QoderTitleDBName),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := QoderTitleDatabase(tc.root)
			assert.True(t, ok)
			assert.Equal(t, tc.want, got)
		})
	}
	for _, tc := range []struct {
		name, root string
	}{
		{"qoderwork", filepath.Join(home, ".qoderwork", "projects")},
		{"custom", filepath.Join(home, "sessions", "projects")},
		{"shared client cache", filepath.Join(home, "Library", "Application Support",
			"Qoder", "SharedClientCache", "cli", "projects")},
		{"project directory itself", filepath.Join(home, ".qoder")},
		{"mirror tail match", filepath.Join(home, "mirror-0001", ".qoder", "projects")},
		{"relative", ".qoder/projects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, ok := QoderTitleDatabase(tc.root)
			assert.False(t, ok, "only a home-anchored default root may bind a store")
			assert.Empty(t, path)
		})
	}
}

func TestQoderSessionRoot(t *testing.T) {
	outer := filepath.Join(string(filepath.Separator), "home", "u", ".qoder", "projects")
	inner := filepath.Join(outer, "nested", ".qoder-cn", "projects")
	transcript := filepath.Join(inner, "-Users-u-demo", "aaaa.jsonl")
	for _, tc := range []struct {
		name, path, want string
		ok               bool
	}{
		{"international", filepath.Join(outer, "-Users-u-demo", "aaaa.jsonl"), outer, true},
		{"subagent", filepath.Join(outer, "-Users-u-demo", "aaaa", "subagents", "agent-1.jsonl"), outer, true},
		{"flat file under root", filepath.Join(outer, "aaaa.jsonl"), outer, true},
		{"most specific root", transcript, inner, true},
		{"no qoder root", filepath.Join("/tmp", "mirror", "projects", "aaaa.jsonl"), "", false},
		{"qoderwork", filepath.Join("/home/u/.qoderwork/projects", "aaaa.jsonl"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, ok := QoderSessionRoot(tc.path)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, root)
		})
	}
}

func TestQoderParseTitlePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		database    bool
		row         string
		sidecar     bool
		rename      bool
		wantName    string
		wantPresent bool
		corrupt     bool
	}{
		{"database beats both files", true, "From database", true, true, "From database", true, false},
		{"missing database sidecar", false, "", true, false, "From sidecar", true, false},
		{"missing database rename", false, "", false, true, "Renamed in chat", true, false},
		{"missing database both files", false, "", true, true, "From sidecar", true, false},
		{"missing database no title", false, "", false, false, "", true, false},
		{"no row sidecar", true, "", true, false, "From sidecar", true, false},
		{"no row rename", true, "", false, true, "Renamed in chat", true, false},
		{"no row both files", true, "", true, true, "From sidecar", true, false},
		{"no row no title", true, "", false, false, "", true, false},
		{"blank row sidecar", true, "   ", true, false, "From sidecar", false, false},
		{"blank row rename", true, "   ", false, true, "Renamed in chat", false, false},
		{"blank row both files", true, "   ", true, true, "From sidecar", false, false},
		{"blank row no title", true, "   ", false, false, "", false, false},
		{"read error sidecar", true, "", true, false, "From sidecar", false, true},
		{"read error rename", true, "", false, true, "Renamed in chat", false, true},
		{"read error both files", true, "", true, true, "From sidecar", false, true},
		{"read error no title", true, "", false, false, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, store := testjsonl.QoderTitleRoot(t)
			if tc.database {
				testjsonl.SetQoderTitle(t, store, "other", "Other session")
			}
			if tc.row != "" {
				testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, tc.row)
			}
			if tc.corrupt {
				require.NoError(t, os.WriteFile(store, []byte("not sqlite"), 0o600))
			}
			path := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "help")
			if tc.rename {
				require.NoError(t, os.WriteFile(path, append(mustRead(t, path), []byte(testjsonl.QoderCustomTitleJSON(testjsonl.QoderTitleID, "Renamed in chat")+"\n")...), 0o644))
			}
			if tc.sidecar {
				require.NoError(t, os.WriteFile(strings.TrimSuffix(path, ".jsonl")+"-session.json", []byte(`{"title":"From sidecar"}`), 0o644))
			}
			results, err := ParseQoderSession(path, "sample-project", "local")
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, tc.wantName, results[0].Session.SessionName)
			assert.Equal(t, tc.wantPresent, results[0].Session.SessionNamePresent)
		})
	}
}

func TestQoderParseSurvivesCorruptSidecar(t *testing.T) {
	t.Run("database still titles it", func(t *testing.T) {
		root, store := testjsonl.QoderTitleRoot(t)
		testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "From database")
		path := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "help")
		require.NoError(t, os.WriteFile(
			path[:len(path)-len(".jsonl")]+"-session.json", []byte("{not json"), 0o644,
		))
		results, err := ParseQoderSession(path, "sample-project", "local")
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, "From database", results[0].Session.SessionName)
		assert.True(t, results[0].Session.SessionNamePresent)
	})
	t.Run("no other signal leaves it blank", func(t *testing.T) {
		root, store := testjsonl.QoderTitleRoot(t)
		testjsonl.SetQoderTitle(t, store, "some-other-session", "Unrelated")
		path := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "help")
		require.NoError(t, os.WriteFile(
			path[:len(path)-len(".jsonl")]+"-session.json", []byte("{not json"), 0o644,
		))
		results, err := ParseQoderSession(path, "sample-project", "local")
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Empty(t, results[0].Session.SessionName)
		assert.True(t, results[0].Session.SessionNamePresent)
	})
}

func TestQoderSubagentTranscriptIsNotTitled(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	testjsonl.SetQoderTitle(t, store, "agent-123", "Subagent name")
	path := filepath.Join(
		root, testjsonl.QoderProjectDir, testjsonl.QoderTitleID, "subagents", "agent-123.jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	// Real prompt content: a content-free /usage probe is dropped by the
	// parser, which would leave nothing here to assert on.
	require.NoError(t, os.WriteFile(path, []byte(
		`{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:27.966Z","message":{"role":"user","content":"inspect the repository"},"sessionId":"agent-123"}`+"\n",
	), 0o644))
	results, _, err := ParseQoderSessionWithExclusions(
		t.Context(), path, "sample-project", "local",
	)
	require.NoError(t, err)
	require.NotEmpty(t, results, "the transcript must parse into a session to prove it stayed untitled")
	for _, result := range results {
		assert.Empty(t, result.Session.SessionName)
		assert.True(t, result.Session.SessionNamePresent)
	}
}

func TestQoderInChatRenameIsATitleSignal(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	path := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "help me")
	require.NoError(t, os.WriteFile(path, append(mustRead(t, path), []byte(testjsonl.QoderCustomTitleJSON(testjsonl.QoderTitleID, "Renamed in chat")+"\n")...), 0o644))
	results, err := ParseQoderSession(path, "sample-project", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "Renamed in chat", results[0].Session.SessionName)
	assert.True(t, results[0].Session.SessionNamePresent,
		"without a title signal the stored name survives and an in-chat rename is lost")

	// The application database still outranks the transcript record.
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Renamed in the app")
	results, err = ParseQoderSession(path, "sample-project", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "Renamed in the app", results[0].Session.SessionName)
}

func TestQoderProviderParseTitlesLocalSourceWithoutRootMetadata(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Renamed")
	testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "help")
	provider, ok := NewProvider(AgentQoder, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	// JSONL sources carry no ConfiguredRoot, so titles cannot depend on one.
	assert.Empty(t, sources[0].ConfiguredRoot)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	session := outcome.Results[0].Result.Session
	assert.Equal(t, "Renamed", session.SessionName)
	assert.True(t, session.SessionNamePresent)
}

func TestQoderParseMaterializedRemoteSource(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Local title")
	path := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "help")
	// A remote import unpacks to a fresh directory whose trailing segments still
	// read ".qoder/projects". Titles must not cross machines: that rewrites the
	// meaning of another host's session_name, not just its display.
	mirror := filepath.Join(t.TempDir(), "mirror-1", ".qoder", "projects",
		testjsonl.QoderProjectDir, testjsonl.QoderTitleID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(mirror), 0o755))
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(mirror, body, 0o644))
	mirrorRoot, ok := QoderSessionRoot(mirror)
	require.True(t, ok)
	_, bound := QoderTitleDatabase(mirrorRoot)
	assert.False(t, bound, "a mirror root must never reach this machine's database")
	results, err := ParseQoderSession(mirror, "sample-project", "host-b")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Session.SessionName)
	assert.True(t, results[0].Session.SessionNamePresent)
}

// TestQoderTitleTargetsOnlyTheFilesOwnSession covers a transcript that yields
// several sessions: the store row is keyed by the file stem, so only that
// session is titled and a fork keeps its own identity.
func TestQoderTitleTargetsOnlyTheFilesOwnSession(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Own title")
	results := []ParseResult{
		{Session: ParsedSession{Agent: AgentQoder, ID: qoderPrefixID(testjsonl.QoderTitleID)}},
		// A real fork ID is the parent session plus the UUID of the entry that
		// starts the branch, so a prefix match would wrongly title it too.
		{Session: ParsedSession{Agent: AgentQoder, ID: qoderPrefixID(
			testjsonl.QoderTitleID + "-22222222-3333-4333-9333-444444444444",
		)}},
	}
	applyQoderTitles(t.Context(), results, filepath.Join(
		root, testjsonl.QoderProjectDir, testjsonl.QoderTitleID+".jsonl",
	), testjsonl.QoderTitleID)
	assert.Equal(t, "Own title", results[0].Session.SessionName)
	assert.True(t, results[0].Session.SessionNamePresent)
	assert.Empty(t, results[1].Session.SessionName)
	assert.False(t, results[1].Session.SessionNamePresent)
}

func TestQoderWatchPlanIncludesTitleStoreDirectory(t *testing.T) {
	root, store := testjsonl.QoderTitleRoot(t)
	provider, ok := NewProvider(AgentQoder, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	dir := filepath.Dir(store)
	titleWatched := func(t *testing.T) bool {
		t.Helper()
		plan, err := provider.WatchPlan(t.Context())
		require.NoError(t, err)
		for _, watched := range plan.Roots {
			if watched.Path != dir {
				continue
			}
			assert.False(t, watched.Recursive)
			assert.Equal(t, "qoder:titles:"+dir, watched.DebounceKey)
			return true
		}
		return false
	}
	// No database, no watch root: a missing optional directory would defer its
	// configured transcript directory from the watched-recovery probe, which is
	// covered by TestQoderMissingTitleDirectoryKeepsTranscriptScopeProbed.
	assert.False(t, titleWatched(t))
	testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, "Renamed")
	assert.True(t, titleWatched(t))
}

func TestQoderStoreTitlesCacheReuseAndRefresh(t *testing.T) {
	_, store := testjsonl.QoderTitleRoot(t)
	testjsonl.SetQoderTitle(t, store, "id", "First")
	cached := readTitles(t, store)
	require.Equal(t, "First", cached["id"])
	reused, _, err := QoderStoreTitles(t.Context(), store)
	require.NoError(t, err)
	// An unchanged container state inside the verify interval reuses the read.
	assert.Equal(t,
		reflect.ValueOf(cached).Pointer(), reflect.ValueOf(reused).Pointer(),
		"the cached map must be returned, not re-read")
	testjsonl.SetQoderTitle(t, store, "id", "Later")
	assert.Equal(t, "Later", readTitles(t, store)["id"])
}

func TestQoderTitleSharesCacheWithAntigravity(t *testing.T) {
	// Both agents read through one path-keyed cache. The same session ID in a
	// Qoder store must not leak into an Antigravity store, so each path keeps
	// its own value for the same ID.
	_, qoderStore := testjsonl.QoderTitleRoot(t)
	testjsonl.SetQoderTitle(t, qoderStore, "shared-id", "")
	antigravityStore := filepath.Join(t.TempDir(), AntigravitySummariesDBName)
	conn, err := sql.Open("sqlite3", antigravityStore)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), testjsonl.AntigravityTitleSchema)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(),
		"INSERT INTO conversation_summaries VALUES ('shared-id', 'Real title')")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	qoderTitles, _, err := QoderStoreTitles(t.Context(), qoderStore)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"shared-id": ""}, qoderTitles)
	antigravityTitles, _, err := AntigravityStoreTitles(t.Context(), antigravityStore)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"shared-id": "Real title"}, antigravityTitles)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	return body
}
