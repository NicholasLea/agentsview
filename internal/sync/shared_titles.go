package sync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// Shared title databases (Antigravity conversation_summaries.db, Qoder
// main.sqlite) are written by the client whenever the user renames a
// conversation and are shared by every session of that client, so a write to
// one is a title-only change for an unknown set of already-imported sessions.
// Parsing the sessions again would rewrite messages and token rows through the
// providers' ForceReplace outcomes; instead the engine refreshes just the
// differing session_name values.
//
// Planning only records which database changed (see ChangedPathPlan
// .SharedTitleTasks and classifyChangedPaths). Every write happens here, under
// syncMu.

// isConfiguredSharedTitleSHMPath claims only the shared-memory sidecar of
// a configured title database. SHM rewrites carry no title changes and must
// not fall through to a provider-wide import. Match paths without statting
// the database, so deleted sidecars remain irrelevant too.
func (e *Engine) isConfiguredSharedTitleSHMPath(path string) bool {
	clean := filepath.Clean(path)
	if !strings.HasSuffix(clean, "-shm") {
		return false
	}
	main := strings.TrimSuffix(clean, "-shm")
	for _, agent := range []parser.AgentType{parser.AgentAntigravity, parser.AgentQoder} {
		if slices.Contains(e.sources().preserveAgents, agent) {
			continue
		}
		for _, root := range e.sources().agentDirs[agent] {
			var expected string
			switch agent {
			case parser.AgentAntigravity:
				expected = filepath.Join(root, "conversation_summaries.db")
			default: // parser.AgentQoder
				expected = parser.QoderTitleDatabaseForLocalSource(root)
			}
			if expected != "" && main == filepath.Clean(expected) {
				return true
			}
		}
	}
	return false
}

// sharedTitleDatabases lists every shared title database reachable from the
// configured roots, across the providers that have one. It is recomputed on
// each sweep so a database that appears later -- a client installed after the
// watcher was armed, or a store deleted and recreated -- is picked up without
// waiting for a filesystem event. Disabled providers are filtered by
// refreshSharedTitleDatabasesLocked, which also guards planned event tasks.
func (e *Engine) sharedTitleDatabases() []parser.SharedTitleDatabase {
	var databases []parser.SharedTitleDatabase
	for _, agent := range []parser.AgentType{
		parser.AgentAntigravity, parser.AgentQoder,
	} {
		roots := e.sources().agentDirs[agent]
		if len(roots) == 0 {
			continue
		}
		databases = append(databases,
			parser.SharedTitleDatabasesForRoots(agent, roots)...)
	}
	return databases
}

// SyncSharedTitlesContext refreshes the titles of already-imported sessions
// from every shared title database reachable from the configured roots,
// without re-reading a single transcript, and returns how many stored names
// changed.
//
// This is the catch-up entry point. The filesystem watcher only observes a
// rename while AgentsView is running and watching, so startup and the
// compensation cycle use this to cover a rename that happened while the
// process was stopped, a title database that appeared after the watcher was
// armed, and a read that failed earlier. Remote full imports call it after
// their transcript scan for the same reason: a title-only rename leaves every
// transcript unchanged, so the scan skips them all. It returns the number of
// refreshed names. A failed read keeps every stored name, is returned to the
// caller, and is retried on the next cycle; it never fabricates a body parse
// retry. Local callers log the error, while remote imports report it so the
// import retries.
func (e *Engine) SyncSharedTitlesContext(ctx context.Context) (int, error) {
	if e.refuseWriteInForceParse("SyncSharedTitles") {
		return 0, nil
	}
	// An archive-only database never reads receiving-host sources, and a
	// shared title store is one. The periodic sweep treats this as a no-op
	// rather than a failure so it does not log on every cycle.
	if e.sourceSyncErr != nil {
		return 0, nil
	}
	e.syncMu.Lock()
	databases := e.sharedTitleDatabases()
	var stats SyncStats
	// Defers run LIFO: the lock is released before the emitter runs, so an
	// Emitter implementation cannot widen the critical section.
	defer func() {
		if stats.hasSessionChanges() {
			e.emit("sessions")
		}
	}()
	defer e.syncMu.Unlock()

	updated, err := e.refreshSharedTitleDatabasesLocked(ctx, databases)
	stats.RecordTitlesUpdated(updated)
	return updated, errors.Join(err, ctx.Err())
}

// reconcileSharedTitlesLocked is the title catch-up of an ordinary local sync.
// A title-only rename leaves every transcript unchanged, so the scan skips
// them all and only this pass can apply the new name. The caller must hold
// syncMu and has already applied the force-parse refusal. Disabled providers
// are skipped by the refresh itself, and an archive-only engine configures no
// roots. A read failure keeps the stored names and is logged; the next sync or
// sweep retries it. Remote importers skip this pass and call
// SyncSharedTitlesContext after their scan, so a failure retries the import.
func (e *Engine) reconcileSharedTitlesLocked(ctx context.Context, stats *SyncStats) {
	if e.pathRewriter != nil || e.idPrefix != "" || ctx.Err() != nil {
		return
	}
	updated, err := e.refreshSharedTitleDatabasesLocked(ctx, e.sharedTitleDatabases())
	stats.RecordTitlesUpdated(updated)
	if err != nil && ctx.Err() == nil {
		log.Printf("sync: shared title refresh: %v", err)
	}
}

// refreshSharedTitleDatabasesLocked applies every planned database refresh and
// returns how many session_name rows actually changed. The caller must hold
// syncMu: it writes the archive database and compares against rows a
// concurrent body sync would also touch.
//
// The database is re-read here rather than trusting values captured while
// planning, so a rename that lands between planning and execution is not lost
// and a stale title is never written.
func (e *Engine) refreshSharedTitleDatabasesLocked(
	ctx context.Context, databases []parser.SharedTitleDatabase,
) (int, error) {
	if e.db.ArchiveContent().UsageOnly() {
		return 0, ctx.Err()
	}
	updated := 0
	var errs error
	for _, database := range databases {
		if database.Agent == parser.AgentQoder && (e.pathRewriter != nil || e.idPrefix != "") {
			continue
		}
		if slices.Contains(e.sources().preserveAgents, database.Agent) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return updated, errors.Join(errs, err)
		}
		records, err := parser.ReadSharedTitles(ctx, database)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf(
				"read shared titles %s: %w", database.DBPath, err,
			))
			continue
		}
		count, err := e.applySharedTitleRecords(ctx, database, records)
		updated += count
		errs = errors.Join(errs, err)
	}
	return updated, errs
}

// applySharedTitleRecords reconciles one database's rows with the stored
// titles. Only a row whose title string differs from the stored value is
// written; a SQL NULL, a missing row, or an unimported session is no signal and
// leaves the stored name alone.
func (e *Engine) applySharedTitleRecords(
	ctx context.Context, database parser.SharedTitleDatabase,
	records []parser.SharedTitleRecord,
) (int, error) {
	prefix := database.IDPrefix()
	updated := 0
	var errs error
	seenRecord := make(map[string]struct{}, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return updated, errors.Join(errs, err)
		}
		if record.ID == "" {
			continue
		}
		if _, ok := seenRecord[record.ID]; ok {
			continue
		}
		seenRecord[record.ID] = struct{}{}
		ownerID := applyIDPrefixToID(e.idPrefix, prefix+record.ID)
		// The database row names the owning transcript. Forks of that file
		// have different session IDs, so refresh every active row that still
		// points at the same source path. If the owning row itself is gone,
		// there is no indexed path to those forks; do not scan the archive.
		sourcePath, _, err := e.db.GetSessionTitleSource(ctx, ownerID)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf(
				"read title source %s: %w", ownerID, err,
			))
			continue
		}
		if sourcePath == "" {
			continue
		}
		ids, err := e.db.ListSessionIDsByFilePath(
			ctx, sourcePath, string(database.Agent),
		)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		seenSession := make(map[string]struct{}, len(ids))
		for _, fullID := range ids {
			if err := ctx.Err(); err != nil {
				return updated, errors.Join(errs, err)
			}
			if _, ok := seenSession[fullID]; ok {
				continue
			}
			seenSession[fullID] = struct{}{}
			current, found, err := e.db.GetSessionName(ctx, fullID)
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf(
					"read stored title %s: %w", fullID, err,
				))
				continue
			}
			if !found {
				continue
			}
			title, present, err := e.sharedTitleForSession(
				ctx, database, fullID, record.Title,
			)
			if err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			if !present {
				continue
			}
			// Stored names are sanitized on every write path, so compare and
			// write the sanitized value; a raw control byte would otherwise
			// rewrite the same displayed name on every sweep.
			title = db.SanitizeUTF8(title)
			if title == current {
				continue
			}
			var value *string
			if title != "" {
				clean := title
				value = &clean
			}
			if err := e.db.RefreshSessionName(ctx, fullID, value); err != nil {
				errs = errors.Join(errs, fmt.Errorf(
					"refresh title %s: %w", fullID, err,
				))
				continue
			}
			updated++
		}
	}
	return updated, errs
}

// sharedTitleForSession resolves the effective title only after confirming
// native local source ownership. JSON recovery is a title result, not a veto.
func (e *Engine) sharedTitleForSession(ctx context.Context, database parser.SharedTitleDatabase, fullID string, fallback *string) (string, bool, error) {
	sourcePath, machine, err := e.db.GetSessionTitleSource(ctx, fullID)
	if err != nil {
		return "", false, err
	}
	switch database.Agent {
	case parser.AgentAntigravity:
		if !e.antigravityTitleOwnsSource(database.DBPath, sourcePath, machine) {
			return "", false, nil
		}
	case parser.AgentQoder:
		// Application Support belongs to this machine, so both the stored
		// attribution and the root's current machine label must name it: a
		// native root relabeled to another machine keeps its older rows'
		// local attribution. Remote engines never reach here; the refresh
		// loop skips Qoder for them before reading the database.
		if (machine != "" && machine != e.machine) || e.machineForPath(parser.AgentQoder, sourcePath) != e.machine {
			return "", false, nil
		}
		expected := parser.QoderTitleDatabaseForLocalSource(sourcePath)
		if !sameLocalFile(database.DBPath, expected) {
			return "", false, nil
		}
		title, present, err := parser.QoderSiblingJSONTitle(sourcePath)
		if err != nil {
			return "", false, fmt.Errorf("read qoder session metadata %s: %w", sourcePath, err)
		}
		if present {
			return title, true, nil
		}
	default:
		return "", false, nil
	}
	if fallback == nil {
		return "", false, nil
	}
	return *fallback, true, nil
}

// antigravityTitleOwnsSource permits local and transported summaries only for
// the IDE root and machine that own the transcript. The owning root is the one
// parsing reads the title from, derived from the transcript's own layout, so a
// nested or sibling root's database never applies, whatever the sweep order.
// Remote stored identities must resolve to a physical source and round-trip
// through the import mapping.
func (e *Engine) antigravityTitleOwnsSource(databasePath, storedPath, machine string) bool {
	root := filepath.Dir(databasePath)
	physical := storedPath
	owner := e.machineForPath(parser.AgentAntigravity, databasePath)
	remote := e.pathRewriter != nil || e.idPrefix != "" || owner != e.machine
	if remote {
		configured := slices.ContainsFunc(e.sources().agentDirs[parser.AgentAntigravity], func(candidate string) bool {
			return filepath.Clean(candidate) == filepath.Clean(root)
		})
		if !configured || machine == "" {
			return false
		}
	}
	if e.pathRewriter != nil {
		if e.storedPathResolver == nil {
			return false
		}
		var ok bool
		physical, ok = e.storedPathResolver(storedPath)
		if !ok || e.pathRewriter(physical) != storedPath {
			return false
		}
	}
	return parser.AntigravityTitleDatabaseForSource(physical) == filepath.Clean(databasePath) &&
		(machine == "" || machine == owner) &&
		e.machineForPath(parser.AgentAntigravity, physical) == owner
}

// sameLocalFile reports whether two spellings name one existing file. A
// watcher can report a database under a linked home through its physical path
// while the native location is spelled under the home directory.
func sameLocalFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	aInfo, err := os.Stat(a)
	if err != nil {
		return false
	}
	bInfo, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(aInfo, bInfo)
}
