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
	{".qoder", "com.qoder.app.stable"},
	{".qoder-cn", "com.qodercn.app.stable"},
}

const qoderTitleQuery = "SELECT session_id, title FROM chat_sessions"

// QoderStoreTitles reads the titles from one Qoder application database.
func QoderStoreTitles(ctx context.Context, path string) (map[string]string, SQLiteContainerState, error) {
	return sqliteStoreTitles(ctx, path, qoderTitleQuery)
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
	switch filepath.Base(filepath.Dir(dir)) {
	case ".qoder", ".qoder-cn":
		return true
	default:
		return false
	}
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

// qoderTitleWatchRoots lists the application directories holding titles. They
// sit outside every session root, so a rename would otherwise never reach a
// sync pass.
//
// A directory is registered only while its database exists, because watching a
// missing one costs more than it buys: the daemon attributes every missing
// provider root to the configured directory it came from and defers that
// directory from the watched-recovery probe (probeWatchRecoveryScope in
// cmd/agentsview/main.go), so an absent optional title directory would drop an
// existing transcript scope from gap reconciliation, overflow recovery, and
// periodic audits. Losing that coverage is worse than a rename waiting for the
// next sync pass, which runs the title sweep anyway.
func qoderTitleWatchRoots(roots []string) []WatchRoot {
	var out []WatchRoot
	seen := make(map[string]bool, len(roots))
	for _, root := range roots {
		store, ok := QoderTitleDatabase(root)
		if !ok {
			continue
		}
		dir := filepath.Dir(store)
		if seen[dir] || !IsRegularFile(store) {
			continue
		}
		seen[dir] = true
		out = append(out, WatchRoot{
			Path:        dir,
			DebounceKey: "qoder:titles:" + dir,
		})
	}
	return out
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
	if !ok || !IsRegularFile(store) {
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
