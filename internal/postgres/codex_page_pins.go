package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// Before data version 127, a thread row could hold a revert page's messages.
// Move its remote pins before replacing that row with the original rollout.
// Publishing the page and moving the pins in this transaction also covers
// pages in a later batch and leaves the old pins intact if publication fails.
func (s *Sync) migrateCodexPagePins(
	ctx context.Context, tx *sql.Tx, batch []db.Session,
	markerID string, legacyMarkerMachines []string,
) error {
	threads := make(map[string]db.Session)
	for _, sess := range batch {
		switch sess.Agent {
		case "codex", "traex", "augure-code":
			prefixEnd := strings.LastIndex(sess.ID, ":") + 1
			if prefixEnd > 0 {
				id := sess.ID[:prefixEnd] + parser.CodexThreadIDFromSessionKey(sess.ID[prefixEnd:])
				if sess.ID == id && sess.DataVersion < 127 {
					continue
				}
				threads[id] = sess
			}
		}
	}
	if len(threads) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, file_path, agent, machine, owner_marker, source_archive_id
		FROM sessions
		WHERE id = ANY($1) AND data_version < 127
		  AND provenance_kind = 'legacy' AND file_path IS NOT NULL
		  AND EXISTS (SELECT 1 FROM pinned_messages WHERE session_id = sessions.id)
		ORDER BY id FOR UPDATE`, mapKeys(threads))
	if err != nil {
		return fmt.Errorf("finding legacy Codex page pins: %w", err)
	}
	type legacyPage struct{ id, path, agent, machine, owner, archive string }
	var pages []legacyPage
	for rows.Next() {
		var page legacyPage
		if err := rows.Scan(&page.id, &page.path, &page.agent, &page.machine, &page.owner, &page.archive); err != nil {
			rows.Close()
			return fmt.Errorf("reading legacy Codex page pins: %w", err)
		}
		pages = append(pages, page)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("reading legacy Codex page pins: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, old := range pages {
		if !sameSessionOwner(
			old.owner, old.machine, markerID,
			pushedSessionMachine(threads[old.id], s.machine), legacyMarkerMachines,
		) {
			continue
		}
		if old.archive != "" && old.archive != s.archiveID {
			return fmt.Errorf("cannot move Codex page pins from another source archive")
		}
		prefixEnd := strings.LastIndex(old.id, ":") + 1
		key := parser.CodexSessionUUIDFromFilename(old.path[strings.LastIndexAny(old.path, `/\`)+1:])
		if key == "" || key == old.id[prefixEnd:] || parser.CodexThreadIDFromSessionKey(key) != old.id[prefixEnd:] {
			continue
		}
		pageID := old.id[:prefixEnd] + key
		// Keep parser-owned names separate from user renames when publishing.
		page, err := s.local.GetArtifactExportSession(ctx, pageID)
		if err != nil {
			return fmt.Errorf("reading retained Codex page: %w", err)
		}
		if page == nil || page.Agent != old.agent {
			return fmt.Errorf("cannot replace pinned Codex thread: retained page %s is unavailable", pageID)
		}
		if projectFailsFilter(page.Project, s.projects, s.excludeProjects) {
			return fmt.Errorf("preserving Codex page pins requires including project %q in the push", page.Project)
		}
		pins, err := snapshotPinnedMessages(ctx, tx, old.id)
		if err != nil {
			return err
		}
		if err := s.pushSession(ctx, tx, *page, markerID, legacyMarkerMachines); err != nil {
			return fmt.Errorf("publishing pinned Codex page: %w", err)
		}
		if _, err := s.pushMessages(ctx, tx, pageID, true, nil, nil); err != nil {
			return err
		}
		if _, err := s.pushSecretFindings(ctx, tx, pageID); err != nil {
			return err
		}
		if err := restorePinnedMessages(ctx, tx, pageID, pins); err != nil {
			return err
		}
	}
	return nil
}
