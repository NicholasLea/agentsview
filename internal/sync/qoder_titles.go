package sync

import (
	"context"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// qoderTitleStore is one application title database and the session root whose
// transcripts it titles. Qoder keeps titles in the app's user-data directory,
// apart from every root, so only the two macOS default roots resolve.
type qoderTitleStore struct {
	root string
	path string
}

func (e *Engine) qoderTitleStores() []qoderTitleStore {
	var stores []qoderTitleStore
	seen := make(map[string]bool)
	for _, root := range e.sources().agentDirs[parser.AgentQoder] {
		path, ok := parser.QoderTitleDatabase(root)
		if !ok || seen[path] {
			continue
		}
		seen[path] = true
		stores = append(stores, qoderTitleStore{root: root, path: path})
	}
	return stores
}

func (e *Engine) reconcileQoderTitlesLocked(ctx context.Context, stats *SyncStats) {
	if _, enabled := e.sources().providerFactories[parser.AgentQoder]; !enabled ||
		e.db.ArchiveContent().UsageOnly() {
		return
	}
	if e.appliedQoderTitles == nil {
		e.appliedQoderTitles = make(map[string]appliedNativeTitleStore)
	}
	for _, store := range e.qoderTitleStores() {
		if ctx.Err() != nil {
			return
		}
		state, exists := parser.StatSQLiteContainerState(store.path)
		now := time.Now()
		if prior, applied := e.appliedQoderTitles[store.path]; applied && exists &&
			prior.state == state &&
			now.Sub(prior.readAt) < parser.SQLiteContainerDigestVerifyInterval {
			continue
		}
		delete(e.appliedQoderTitles, store.path)
		parser.InvalidateQoderTitles(store.path)
		titles, state, err := parser.QoderStoreTitles(ctx, store.path)
		if err == nil {
			var n int
			n, err = e.applyQoderStoreTitles(ctx, store.root, titles)
			stats.TitlesUpdated += n
		}
		if err != nil {
			stats.RecordFailed()
			if retry, ok := ctx.Value(nativeTitleRetryContextKey{}).(*WatchBatch); ok {
				retry.Paths = watchAppendUniqueString(retry.Paths, store.path)
			}
			parser.InvalidateQoderTitles(store.path)
			log.Printf("qoder titles %s: %v", store.path, err)
			continue
		}
		if state != (parser.SQLiteContainerState{}) {
			e.appliedQoderTitles[store.path] = appliedNativeTitleStore{state: state, readAt: now}
		}
	}
}

// applyQoderStoreTitles renames ingested sessions by their own IDs. A blank or
// missing title carries no signal: unlike Antigravity, Qoder has no way to
// express "the user cleared this", so a blank must not erase a name the
// sidecar or an earlier read supplied.
func (e *Engine) applyQoderStoreTitles(
	ctx context.Context, root string, titles map[string]string,
) (updated int, err error) {
	if len(titles) == 0 {
		return 0, nil
	}
	prefix := e.idPrefix + string(parser.AgentQoder) + ":"
	ids := make([]string, 0, len(titles))
	for id := range maps.Keys(titles) {
		ids = append(ids, prefix+id)
	}
	slices.Sort(ids)
	stored, err := e.db.ListSessionWriteIdentitiesByID(ctx, ids)
	if err != nil {
		return 0, err
	}
	var archived map[string]db.SessionWriteIdentity
	if reader, ok := e.archiveStore.(sessionWriteIdentityReader); ok &&
		e.deferredNativeTitles != nil {
		// A rebuild copies archived sessions without sources only after this pass.
		if archived, err = reader.ListSessionWriteIdentitiesByID(ctx, ids); err != nil {
			return 0, err
		}
	}
	for _, id := range ids {
		session, ok := stored[id]
		deferred := false
		if !ok {
			session, deferred = archived[id]
		}
		if !ok && !deferred {
			continue
		}
		title := db.SanitizeUTF8(titles[strings.TrimPrefix(id, prefix)])
		if title == "" || session.SessionName != nil && *session.SessionName == title {
			continue
		}
		if !e.qoderRootOwns(root, session.FilePath) || !e.cwdFilter.allows(session.Cwd) {
			continue
		}
		if deferred {
			e.deferredNativeTitles[id] = &title
			continue
		}
		if err := e.db.RefreshNativeSessionName(ctx, id, &title); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

// qoderRootOwns keeps one root's database from renaming a same-ID session that
// belongs to another configured root.
func (e *Engine) qoderRootOwns(root, storedPath string) bool {
	if e.pathRewriter != nil {
		if e.storedPathResolver == nil {
			return false
		}
		resolved, ok := e.storedPathResolver(storedPath)
		if !ok {
			return false
		}
		storedPath = resolved
	}
	owner, ok := parser.QoderSessionRoot(storedPath)
	return ok && sameReconciliationSourcePath(owner, root)
}
