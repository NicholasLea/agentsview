package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestQoderStoreTitles(t *testing.T) {
	_, store := testjsonl.QoderTitleRoot(t)
	testjsonl.SetQoderTitle(t, store, "plain-id", "  Renamed  ")
	testjsonl.SetQoderTitle(t, store, "blank-id", "   ")
	// Leave a WAL-mode database with no sibling files, as when Qoder is closed.
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
	titles, _, err := QoderStoreTitles(t.Context(), store)
	require.NoError(t, err)
	assert.Equal(t, "Renamed", titles["plain-id"], "outer whitespace is trimmed")
	// Asserted through the lookup result rather than the value alone: a missing
	// key would satisfy an emptiness check on its own.
	blank, blankRow := titles["blank-id"]
	assert.True(t, blankRow, "a whitespace-only row stays in the map for the parse to see")
	assert.Empty(t, blank)
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
		blocked     bool
		schema      string
	}{
		{name: "database beats both files", database: true, row: "From database", sidecar: true, rename: true, wantName: "From database", wantPresent: true},
		{name: "missing database sidecar", sidecar: true, wantName: "From sidecar", wantPresent: true},
		{name: "missing database rename", rename: true, wantName: "Renamed in chat", wantPresent: true},
		{name: "missing database both files", sidecar: true, rename: true, wantName: "From sidecar", wantPresent: true},
		{name: "missing database no title"},
		{name: "missing table sidecar", schema: "CREATE TABLE settings (value TEXT)", sidecar: true, wantName: "From sidecar", wantPresent: true},
		{name: "no row sidecar", database: true, sidecar: true, wantName: "From sidecar", wantPresent: true},
		{name: "no row no title", database: true},
		{name: "blank row sidecar", database: true, row: "   ", sidecar: true, wantName: "From sidecar"},
		{name: "blank row no title", database: true, row: "   "},
		{name: "sanitized blank row sidecar", database: true, row: "\x00  \x00", sidecar: true, wantName: "From sidecar"},
		{name: "stripped row sidecar", database: true, row: "\x00\x1b\u0080\x7f", sidecar: true, wantName: "From sidecar"},
		{name: "stripped row no title", database: true, row: "\x00\x1b\u0080\x7f"},
		{name: "sanitized database title", database: true, row: "\x00Real\t日本語\nTitle\x1b\u0080", sidecar: true, wantName: "Real\t日本語\nTitle", wantPresent: true},
		{name: "stat error sidecar", database: true, sidecar: true, blocked: true, wantName: "From sidecar"},
		{name: "read error sidecar", database: true, sidecar: true, wantName: "From sidecar", corrupt: true},
		{name: "read error no title", database: true, corrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, store := testjsonl.QoderTitleRoot(t)
			if tc.schema != "" {
				require.NoError(t, os.MkdirAll(filepath.Dir(store), 0o755))
				conn, err := sql.Open("sqlite3", store)
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), tc.schema)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
			}
			if tc.database {
				testjsonl.SetQoderTitle(t, store, "other", "Other session")
			}
			if tc.row != "" {
				testjsonl.SetQoderTitle(t, store, testjsonl.QoderTitleID, tc.row)
			}
			if tc.corrupt {
				require.NoError(t, os.WriteFile(store, []byte("not sqlite"), 0o600))
			}
			if tc.blocked {
				if runtime.GOOS == "windows" || os.Geteuid() == 0 {
					t.Skip("requires Unix permissions enforced for a non-root user")
				}
				dir := filepath.Dir(store)
				require.NoError(t, os.Chmod(dir, 0o000))
				t.Cleanup(func() { require.NoError(t, os.Chmod(dir, 0o755)) })
				_, err := os.Lstat(store)
				require.ErrorIs(t, err, os.ErrPermission)
			}
			path := testjsonl.WriteQoderTranscript(t, root, testjsonl.QoderTitleID, "help")
			if tc.rename {
				body, err := os.ReadFile(path)
				require.NoError(t, err)
				rename := `{"type":"custom-title","sessionId":"` + testjsonl.QoderTitleID + `","customTitle":"Renamed in chat"}` + "\n"
				require.NoError(t, os.WriteFile(path, append(body, []byte(rename)...), 0o644))
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
		assert.False(t, result.Session.SessionNamePresent)
	}
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
	results, err := ParseQoderSession(mirror, "sample-project", "host-b")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Session.SessionName)
	assert.False(t, results[0].Session.SessionNamePresent)
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
