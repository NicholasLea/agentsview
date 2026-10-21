package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	return qoderSchemalessTitles(sqliteStoreTitles(ctx, path, qoderTitleQuery))
}

// qoderSchemalessTitles reports an application database without a
// chat_sessions title schema as holding no titles, so a general app database
// without them is not a read failure.
func qoderSchemalessTitles(
	titles map[string]string, state SQLiteContainerState, err error,
) (map[string]string, SQLiteContainerState, error) {
	if err != nil && (strings.Contains(err.Error(), "no such table: chat_sessions") ||
		strings.Contains(err.Error(), "no such column:")) {
		return nil, state, nil
	}
	return titles, state, err
}

// QoderSessionRoot returns the Qoder session root that owns a transcript path,
// even after the file was deleted. Roots are matched innermost-out, so a root
// nested inside another root claims its own transcripts.
func QoderSessionRoot(path string) (string, bool) {
	dir := filepath.Dir(filepath.Clean(path))
	for {
		if isQoderSessionRoot(dir) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func isQoderSessionRoot(dir string) bool {
	base := filepath.Base(dir)
	if base != "projects" {
		return false
	}
	dotDir := filepath.Base(filepath.Dir(dir))
	for _, layout := range qoderTitleLayouts {
		if dotDir == layout.dotDir {
			return true
		}
	}
	return false
}

// QoderTitleDatabase returns the application database holding titles for a
// configured session root. Only the two macOS default roots have a known
// title store: a custom root, a Qoder CLI or .qoderwork root, and a remote
// mirror root extracted under a temp directory all return false. Matching a
// trailing path segment would bind the local database to another machine's
// sessions, so the comparison is anchored to this user's home directory.
func QoderTitleDatabase(root string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	clean := filepath.Clean(root)
	for _, layout := range qoderTitleLayouts {
		if clean == filepath.Join(home, layout.dotDir, "projects") {
			store := filepath.Join(
				home, "Library", "Application Support", layout.appID, QoderTitleDBName,
			)
			return store, true
		}
	}
	return "", false
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
