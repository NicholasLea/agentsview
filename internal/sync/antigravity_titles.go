package sync

import (
	"context"
	"log"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type antigravityTitleState struct {
	state  parser.SQLiteContainerState
	readAt time.Time
}

func (e *Engine) invalidateAntigravityTitleStates(paths []string) {
	for _, path := range paths {
		delete(e.antigravityTitleStates, strings.TrimSuffix(filepath.Clean(path), "-wal"))
	}
}

// The caller holds syncMu and reconciles after parsing so an older parsed title cannot win.
func (e *Engine) reconcileAntigravityTitlesLocked(ctx context.Context, fresh bool) int {
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
		now := openCodeContainerDigestVerifyNow()
		if prior, seen := e.antigravityTitleStates[store]; !fresh && seen && prior.state == state && now.Sub(prior.readAt) >= 0 && now.Sub(prior.readAt) < openCodeContainerDigestVerifyInterval {
			continue
		}
		titles, err := parser.AntigravityStoreTitles(ctx, store, true)
		if err != nil {
			if ctx.Err() != nil {
				return updated
			}
			log.Printf("antigravity titles %s: %v", store, err)
		} else {
			n, err := e.applyAntigravityStoreTitles(ctx, root, titles)
			updated += n
			if err != nil {
				delete(e.antigravityTitleStates, store)
				log.Printf("antigravity titles %s: %v", store, err)
				continue
			}
		}
		if e.antigravityTitleStates == nil {
			e.antigravityTitleStates = make(map[string]antigravityTitleState)
		}
		e.antigravityTitleStates[store] = antigravityTitleState{state: state, readAt: now}
	}
	return updated
}

// Unreadable stores settle too; archive write failures remain pending for retry.
func (e *Engine) SnapshotAntigravityTitleStates() map[string]parser.SQLiteContainerState {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	states := make(map[string]parser.SQLiteContainerState, len(e.antigravityTitleStates))
	for path, prior := range e.antigravityTitleStates {
		states[path] = prior.state
	}
	return states
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
