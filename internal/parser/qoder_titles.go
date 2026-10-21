package parser

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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
	{".qoder", "com.qoder.app.stable"},
	{".qoder-cn", "com.qodercn.app.stable"},
}

var qoderTitleStore = sqliteTitleStore{
	// Only these two columns are read: the two builds' tables are not
	// column-identical, and a missing column must surface as an error that
	// leaves stored titles alone.
	//
	// Deleted and archived rows are included because a title is still the
	// title; only sessions AgentsView has ingested are ever looked up by ID.
	query:                 "SELECT session_id, title FROM chat_sessions",
	immutableWhenWALEmpty: true,
}

// QoderStoreTitles reads the titles from one Qoder application database.
func QoderStoreTitles(ctx context.Context, path string) (map[string]string, SQLiteContainerState, error) {
	return sqliteStoreTitles(ctx, path, qoderTitleStore)
}

func InvalidateQoderTitles(path string) {
	invalidateSQLiteTitles(path)
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
	if runtime.GOOS != "darwin" {
		return "", false
	}
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
// sync pass. A directory is listed whether or not its database exists yet:
// missing roots stay in the watch plan, and a lifecycle-aware backend adds
// ancestor coverage so the directory's creation is reported once Qoder makes
// it. The periodic title pass still covers a database appearing inside a
// directory that is already watched.
func qoderTitleWatchRoots(roots []string) []WatchRoot {
	var out []WatchRoot
	seen := make(map[string]bool, len(roots))
	for _, root := range roots {
		store, ok := QoderTitleDatabase(root)
		if !ok {
			continue
		}
		dir := filepath.Dir(store)
		if seen[dir] {
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

// qoderSessionTitleID returns the chat_sessions key for a transcript path, or
// "" when the file is not a session's own transcript. Subagent files are
// excluded by the caller's subagent gate, not by a second copy of it here.
func qoderSessionTitleID(path string) string {
	stem, ok := strings.CutSuffix(filepath.Base(path), ".jsonl")
	if !ok || !IsValidQoderSessionID(stem) {
		return ""
	}
	return stem
}

// applyQoderTitles takes titles from the root's application database. The title
// sources rank: a title the database holds, then a rename recorded in the
// transcript, then the `-session.json` snapshot, which is export-time metadata
// and only seeds a first import. A blank row or no row means the database says
// nothing, so the snapshot claims no signal either and the stored name
// survives. An unreadable database reports nothing at all, and reconciliation
// retries it. Only the session the transcript is named for is titled; forks
// parsed from the same file keep their own identity.
func applyQoderTitles(ctx context.Context, results []ParseResult, path string) {
	root, ok := QoderSessionRoot(path)
	if !ok {
		return
	}
	store, ok := QoderTitleDatabase(root)
	if !ok {
		return
	}
	id := qoderSessionTitleID(path)
	if id == "" {
		return
	}
	titles, _, err := QoderStoreTitles(ctx, store)
	if err != nil {
		// A store that cannot be read supplies no signal, so the pass that
		// read it can restore the title later. Reconciliation retries it.
		return
	}
	title, row := titles[id]
	target := qoderPrefixID(id)
	for i := range results {
		if results[i].Session.ID != target {
			continue
		}
		if row && title != "" {
			results[i].Session.SessionName = title
			results[i].Session.SessionNamePresent = true
			continue
		}
		// The database holds this session and has no usable title for it, either
		// a blank row or no row at all. Reconciliation cannot heal either case,
		// so an older export snapshot must not replace the name already stored.
		// No signal keeps that name on update and still seeds a first import,
		// while a rename recorded in the transcript outranks the blank row.
		if !results[i].Session.claudeRenameSeen {
			results[i].Session.SessionNamePresent = false
		}
	}
}
