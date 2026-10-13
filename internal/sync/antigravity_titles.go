package sync

import (
	"context"
	"log"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// The caller holds syncMu and reconciles after parsing so an older parsed title cannot win.
func (e *Engine) reconcileAntigravityTitlesLocked(ctx context.Context) int {
	sources := e.sources()
	if _, enabled := sources.providerFactories[parser.AgentAntigravity]; !enabled ||
		e.db.ArchiveContent().UsageOnly() {
		return 0
	}
	updated := 0
	for _, root := range sources.agentDirs[parser.AgentAntigravity] {
		if ctx.Err() != nil {
			return updated
		}
		store := filepath.Join(root, parser.AntigravitySummariesDBName)
		state, ok := parser.StatSQLiteContainerState(store)
		if !ok {
			continue
		}
		if prior, seen := e.antigravityTitleStates[store]; seen && prior == state {
			continue
		}
		n, err := e.applyAntigravityStoreTitles(ctx, root, store)
		updated += n
		if err != nil {
			log.Printf("antigravity titles %s: %v", store, err)
			continue
		}
		if e.antigravityTitleStates == nil {
			e.antigravityTitleStates = make(map[string]parser.SQLiteContainerState)
		}
		e.antigravityTitleStates[store] = state
	}
	return updated
}

// SnapshotAntigravityTitleStates lets remote mirrors record only successfully applied stores.
func (e *Engine) SnapshotAntigravityTitleStates() map[string]parser.SQLiteContainerState {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	return maps.Clone(e.antigravityTitleStates)
}

func (e *Engine) applyAntigravityStoreTitles(ctx context.Context, root, store string) (updated int, err error) {
	titles, err := parser.AntigravityStoreTitles(ctx, store, true)
	if err != nil || len(titles) == 0 {
		return 0, err
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
	if storedPath == "" {
		return false
	}
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
