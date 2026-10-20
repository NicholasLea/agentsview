package sync

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

// nativeTitleAgents lists the providers whose session titles live outside the
// transcript. Each is reconciled and invalidated independently so disabling
// one never stops the other from picking up renames.
func (e *Engine) reconcileNativeTitlesLocked(ctx context.Context, stats *SyncStats) {
	e.reconcileAntigravityTitlesLocked(ctx, stats)
	e.reconcileQoderTitlesLocked(ctx, stats)
}

func (e *Engine) invalidateNativeTitleCache(paths []string) {
	e.invalidateAntigravityTitleCache(paths)
	e.invalidateQoderTitleCache(paths)
}

// recordNativeTitleUpdates folds a title-only pass into the reported sync
// state. Titles change without any session being written, so a pass that only
// renamed sessions still counts as activity.
func (e *Engine) recordNativeTitleUpdates(stats SyncStats) {
	if stats.TitlesUpdated > 0 || stats.Failed > 0 {
		e.mu.Lock()
		if stats.TitlesUpdated > 0 && stats.ProcessingComplete() {
			e.lastSync = time.Now()
		}
		e.lastSyncStats.TitlesUpdated += stats.TitlesUpdated
		e.lastSyncStats.Failed += stats.Failed
		e.mu.Unlock()
	}
}

// invalidateQoderTitleCache drops cached reads for the Qoder application
// databases named by a changed-path batch. Only a store this engine can reach
// counts, so a main.sqlite elsewhere on the machine is not one.
func (e *Engine) invalidateQoderTitleCache(paths []string) {
	if paths == nil {
		clear(e.appliedQoderTitles)
		for _, store := range e.qoderTitleStores() {
			parser.InvalidateQoderTitles(store.path)
		}
		return
	}
	reachable := make(map[string]bool)
	for _, store := range e.qoderTitleStores() {
		reachable[store.path] = true
	}
	for _, path := range paths {
		store := strings.TrimSuffix(filepath.Clean(path), "-wal")
		if !reachable[store] {
			continue
		}
		// A read-only connection creates and deletes an empty WAL on open and
		// close, and rewrites the `-shm` index. Neither says anything about the
		// titles, and invalidating on them would make each refresh emit the very
		// event that schedules the next one.
		if _, ok := parser.SQLiteContainerPathForEvent(
			filepath.Dir(store), path, parser.QoderTitleDBName, true,
		); !ok {
			continue
		}
		delete(e.appliedQoderTitles, store)
		parser.InvalidateQoderTitles(store)
	}
}
