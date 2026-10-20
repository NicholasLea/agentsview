package parser

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"
)

// sqliteTitleSnapshot is one cached read of a native title database.
type sqliteTitleSnapshot struct {
	state  SQLiteContainerState
	titles map[string]string
	err    error
	readAt time.Time
}

var sqliteTitleNow = time.Now

// One cache serves every native title database. Two copies of the expiry and
// container-state rules would drift apart.
var sqliteTitleCache = struct {
	sync.Mutex
	stores map[string]sqliteTitleSnapshot
}{stores: make(map[string]sqliteTitleSnapshot)}

// sqliteTitleStore describes how to read one agent-owned title database.
// A store path is only ever read through one of these, so the cache can be
// keyed by path alone.
type sqliteTitleStore struct {
	// query selects (session id, title) rows.
	query string
	// immutableWhenWALEmpty opens a snapshot without WAL locking once the
	// sibling WAL carries no frames. Qoder's database sits in an Electron
	// user-data directory and may have no `-wal`/`-shm` siblings at all when
	// the app is not running, which plain `mode=ro` refuses to open.
	//
	// shortcut: the app can create its first `-wal` between the check and the
	// read, so this open may see a torn page. A read error keeps the stored
	// title, and the 5-minute expiry re-reads on the next pass.
	immutableWhenWALEmpty bool
	// keepBlankTitles stores an empty title so the sync pass can clear a
	// name it previously applied. Qoder has no "the user cleared this title"
	// signal, so a blank there carries no information and must not erase a
	// name that came from the sidecar or the transcript.
	keepBlankTitles bool
}

func invalidateSQLiteTitles(path string) {
	sqliteTitleCache.Lock()
	defer sqliteTitleCache.Unlock()
	delete(sqliteTitleCache.stores, path)
}

// sqliteStoreTitles reads a native title database, reusing the last result
// while the container state matches. Matching markers can hide writes, so
// cached titles expire.
func sqliteStoreTitles(
	ctx context.Context, path string, store sqliteTitleStore,
) (map[string]string, SQLiteContainerState, error) {
	state, ok := StatSQLiteContainerState(path)
	sqliteTitleCache.Lock()
	defer sqliteTitleCache.Unlock()
	now := sqliteTitleNow()
	if prior, cached := sqliteTitleCache.stores[path]; ok && cached &&
		prior.state == state &&
		now.Sub(prior.readAt) < SQLiteContainerDigestVerifyInterval {
		return prior.titles, prior.state, prior.err
	}
	titles, err := readSQLiteTitles(ctx, path, store)
	if ctx.Err() != nil {
		return nil, state, ctx.Err()
	}
	if ok {
		// Remote imports extract stores into fresh temp roots; bound what stays cached.
		if _, cached := sqliteTitleCache.stores[path]; !cached && len(sqliteTitleCache.stores) >= 16 {
			clear(sqliteTitleCache.stores)
		}
		sqliteTitleCache.stores[path] = sqliteTitleSnapshot{
			state: state, titles: titles, err: err, readAt: now,
		}
	} else {
		delete(sqliteTitleCache.stores, path)
	}
	return titles, state, err
}

func readSQLiteTitles(
	ctx context.Context, path string, store sqliteTitleStore,
) (map[string]string, error) {
	if !IsRegularFile(path) {
		return nil, nil
	}
	observeSharedContainerScan(ctx)
	options := sqliteReadOptions{busyTimeoutMS: 3000}
	if store.immutableWhenWALEmpty && !sqliteWALHasFrames(path+"-wal") {
		options = sqliteReadOptions{stableSnapshot: true}
	}
	db, err := openSQLiteReadOnly(path, options)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, store.query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	titles := make(map[string]string)
	for rows.Next() {
		var id string
		var title sql.NullString
		if err := rows.Scan(&id, &title); err != nil {
			return nil, err
		}
		if !title.Valid {
			continue
		}
		trimmed := strings.TrimSpace(title.String)
		if trimmed == "" && !store.keepBlankTitles {
			continue
		}
		titles[id] = trimmed
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return titles, nil
}
