package parser

import (
	"context"
	"os"
	"path/filepath"
)

// QoderTitleDBName is the database where the Qoder IDE stores session titles.
// It lives in the app's Electron user-data directory, outside every session
// root, so a root is bound to it only by an absolute home-anchored path.
const QoderTitleDBName = "main.sqlite"

// qoderTitleLayouts pairs each macOS session root with the app that owns it.
// The two builds keep separate databases, and one build's session ID is looked
// up only under its own root.
var qoderTitleLayouts = []struct {
	dotDir string
	appID  string
}{
	{qoderDotDir, "com.qoder.app.stable"},
	{qoderCNDotDir, "com.qodercn.app.stable"},
}

const qoderTitleQuery = "SELECT session_id, title FROM chat_sessions"

// QoderStoreTitles reads the titles from one Qoder application database.
func QoderStoreTitles(ctx context.Context, path string) (map[string]string, SQLiteContainerState, error) {
	return sqliteStoreTitles(ctx, path, "chat_sessions", qoderTitleQuery)
}

// QoderSessionRoot returns the canonical default root that owns a transcript
// path, even after the file was deleted. A nested default root takes precedence.
func QoderSessionRoot(path string) (string, bool) {
	path, _, err := ResolveProviderRoot(AgentQoder, path)
	if err != nil {
		return "", false
	}
	owner := ""
	for root := range qoderTitleDatabases() {
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != "." && filepath.IsLocal(rel) && len(root) > len(owner) {
			owner = root
		}
	}
	return owner, owner != ""
}

// QoderTitleDatabase returns the application database holding titles for a
// configured session root. Only the two macOS default roots have a known
// title store: a custom root, a Qoder CLI or .qoderwork root, and a remote
// mirror root extracted under a temp directory all return false. Matching a
// trailing path segment would bind the local database to another machine's
// sessions, so the comparison is anchored to this user's home directory.
func QoderTitleDatabase(root string) (string, bool) {
	if !filepath.IsAbs(root) {
		return "", false
	}
	root, _, err := ResolveProviderRoot(AgentQoder, root)
	if err != nil {
		return "", false
	}
	store, ok := qoderTitleDatabases()[root]
	return store, ok
}

func qoderTitleDatabases() map[string]string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	stores := make(map[string]string, len(qoderTitleLayouts))
	for _, layout := range qoderTitleLayouts {
		root, _, err := ResolveProviderRoot(AgentQoder, filepath.Join(home, layout.dotDir, "projects"))
		if err != nil {
			continue
		}
		stores[root] = filepath.Join(home, "Library", "Application Support", layout.appID, QoderTitleDBName)
	}
	return stores
}

// applyQoderTitles uses a non-blank app database title for the file's own
// session. A successful read with no row preserves the parser's title signal.
// A blank row or read error suppresses that signal, so file-side names seed
// inserts while updates preserve the archived name.
func applyQoderTitles(ctx context.Context, results []ParseResult, path, fileStem string) {
	root, ok := QoderSessionRoot(path)
	if !ok {
		return
	}
	store, ok := QoderTitleDatabase(root)
	if !ok {
		return
	}
	for i := range results {
		results[i].Session.SessionNamePresent = results[i].Session.SessionName != ""
	}
	titles, _, err := QoderStoreTitles(ctx, store)
	title, row := "", false
	if err == nil {
		title, row = titles[fileStem]
	}
	if err == nil && !row {
		return
	}
	target := qoderPrefixID(fileStem)
	for i := range results {
		if results[i].Session.ID != target {
			continue
		}
		if row && title != "" {
			results[i].Session.SessionName = title
			results[i].Session.SessionNamePresent = true
			continue
		}
		results[i].Session.SessionNamePresent = false
	}
}
