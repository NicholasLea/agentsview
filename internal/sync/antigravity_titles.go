package sync

import (
	"context"
	"log"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// reconcileAntigravityTitlesLocked copies titles from each Antigravity root's
// title store onto its stored sessions. A rename never touches the transcript,
// so this covers renames, sessions whose transcript is gone, and sessions
// imported before titles were read. Unchanged stores are skipped unless force
// is set; an unreadable store keeps stored titles and is retried next pass.
// The caller holds syncMu and runs this after the pass's parse writes, so a
// parse that read an older title cannot overwrite a newer one.
func (e *Engine) reconcileAntigravityTitlesLocked(ctx context.Context, force bool) int {
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
		if prior, seen := e.antigravityTitleStates[store]; seen && !force && prior == state {
			continue
		}
		n, trashedStale, err := e.applyAntigravityStoreTitles(ctx, root, store)
		updated += n
		if err != nil {
			log.Printf("antigravity titles %s: %v", store, err)
			continue
		}
		// A trashed session can be restored without a store change; keep rereading.
		if trashedStale {
			continue
		}
		if e.antigravityTitleStates == nil {
			e.antigravityTitleStates = make(map[string]parser.SQLiteContainerState)
		}
		e.antigravityTitleStates[store] = state
	}
	return updated
}

func (e *Engine) applyAntigravityStoreTitles(ctx context.Context, root, store string) (updated int, trashedStale bool, err error) {
	titles, err := parser.AntigravityStoreTitles(ctx, store, true)
	if err != nil || len(titles) == 0 {
		return 0, false, err
	}
	prefix := e.idPrefix + string(parser.AgentAntigravity) + ":"
	ids := make([]string, 0, len(titles))
	for id := range maps.Keys(titles) {
		ids = append(ids, prefix+id)
	}
	slices.Sort(ids)
	stored, err := e.db.ListSessionWriteIdentitiesByID(ctx, ids)
	if err != nil {
		return 0, false, err
	}
	for _, id := range ids {
		session, ok := stored[id]
		if !ok || session.Agent != string(parser.AgentAntigravity) {
			continue
		}
		title := titles[strings.TrimPrefix(id, prefix)]
		if session.SessionName != nil && *session.SessionName == title ||
			session.SessionName == nil && title == "" {
			continue
		}
		if !e.antigravityRootOwns(root, session.FilePath) {
			continue
		}
		if session.Deleted {
			trashedStale = true
			continue
		}
		var name *string
		if title != "" {
			name = &title
		}
		changed, err := e.db.RefreshActiveSessionName(ctx, id, name)
		if err != nil {
			return updated, false, err
		}
		if !changed {
			// Trashed after the lookup; retry so a restore still gets the title.
			trashedStale = true
			continue
		}
		updated++
	}
	return updated, trashedStale, nil
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
