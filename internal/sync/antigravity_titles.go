package sync

import (
	"context"
	"errors"
	"log"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func (e *Engine) invalidateAntigravityTitleCache(paths []string) {
	if paths == nil {
		for _, root := range e.sources().agentDirs[parser.AgentAntigravity] {
			parser.InvalidateAntigravityTitles(filepath.Join(root, parser.AntigravitySummariesDBName))
		}
	}
	for _, path := range paths {
		parser.InvalidateAntigravityTitles(strings.TrimSuffix(filepath.Clean(path), "-wal"))
	}
}

// Reconcile after parsing so an older parsed title cannot win.
func (e *Engine) reconcileAntigravityTitlesLocked(ctx context.Context, stats *SyncStats) error {
	sources := e.sources()
	if _, enabled := sources.providerFactories[parser.AgentAntigravity]; !enabled || e.db.ArchiveContent().UsageOnly() {
		return nil
	}
	var writeErr error
	for _, root := range sources.agentDirs[parser.AgentAntigravity] {
		if ctx.Err() != nil {
			return errors.Join(writeErr, ctx.Err())
		}
		store := filepath.Join(root, parser.AntigravitySummariesDBName)
		titles, err := parser.AntigravityStoreTitles(ctx, store)
		if err != nil {
			if ctx.Err() != nil {
				return errors.Join(writeErr, ctx.Err())
			}
			log.Printf("antigravity titles %s: %v", store, err)
			continue
		}
		n, err := e.applyAntigravityStoreTitles(ctx, root, titles)
		stats.TitlesUpdated += n
		if err != nil {
			stats.RecordFailed()
			parser.InvalidateAntigravityTitles(store)
			writeErr = errors.Join(writeErr, err)
			log.Printf("antigravity titles %s: %v", store, err)
		}
	}
	return writeErr
}

func (e *Engine) SnapshotAntigravityTitleStates() map[string]parser.SQLiteContainerState {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	states := make(map[string]parser.SQLiteContainerState)
	for _, root := range e.sources().agentDirs[parser.AgentAntigravity] {
		path := filepath.Join(root, parser.AntigravitySummariesDBName)
		if state, ok := parser.AntigravityTitleStoreState(path); ok {
			states[path] = state
		}
	}
	return states
}

func (e *Engine) recordAntigravityTitleUpdates(stats SyncStats) {
	if stats.TitlesUpdated > 0 && stats.ProcessingComplete() {
		e.mu.Lock()
		e.lastSync = time.Now()
		e.lastSyncStats.TitlesUpdated += stats.TitlesUpdated
		e.mu.Unlock()
	}
}

func (e *Engine) applyAntigravityStoreTitles(ctx context.Context, root string, titles map[string]string) (updated int, err error) {
	if len(titles) == 0 {
		return 0, nil
	}
	prefix := e.idPrefix + string(parser.AgentAntigravity) + ":"
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
	if reader, ok := e.archiveStore.(sessionWriteIdentityReader); ok && e.deferredAntigravityTitles != nil {
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
		if session.SessionName != nil && *session.SessionName == title ||
			session.SessionName == nil && title == "" {
			continue
		}
		if !e.antigravityRootOwns(root, session.FilePath) {
			continue
		}
		var name *string
		if title != "" {
			name = &title
		}
		if deferred {
			e.deferredAntigravityTitles[id] = name
			continue
		}
		// Trashed sessions take the title too, so restoring one needs no store change.
		if err := e.db.RefreshNativeSessionName(ctx, id, name); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

// antigravityRootOwns keeps a store from renaming a same-ID session that
// belongs to another configured root.
func (e *Engine) antigravityRootOwns(root, storedPath string) bool {
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
	owner, ok := parser.AntigravitySessionRoot(storedPath)
	return ok && sameReconciliationSourcePath(owner, root)
}
