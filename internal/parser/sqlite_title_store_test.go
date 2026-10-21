package parser

import (
	"bytes"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestSQLiteTitleMissingColumnWarnsOnStateChange(t *testing.T) {
	for _, column := range []string{"conversation_id", "title"} {
		t.Run(column, func(t *testing.T) {
			store := filepath.Join(t.TempDir(), AntigravitySummariesDBName)
			conn, err := sql.Open("sqlite3", store)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), testjsonl.AntigravityTitleSchema)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), "ALTER TABLE conversation_summaries RENAME COLUMN "+column+" TO unavailable_column")
			require.NoError(t, err)
			require.NoError(t, conn.Close())

			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(previous) })
			now := time.Now()
			previousNow := sqliteTitleNow
			sqliteTitleNow = func() time.Time { return now }
			t.Cleanup(func() { sqliteTitleNow = previousNow })
			titles, state, err := AntigravityStoreTitles(t.Context(), store)
			require.NoError(t, err)
			assert.Empty(t, titles, "a renamed required column yields no titles")
			assert.Contains(t, logs.String(), "Warning:")
			assert.Contains(t, logs.String(), store)
			assert.Contains(t, logs.String(), "missing column "+column)
			assert.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("Warning:")))

			logs.Reset()
			for _, refresh := range []string{"cached", "expired", "invalidated"} {
				if refresh == "expired" {
					now = now.Add(SQLiteContainerDigestVerifyInterval)
				}
				if refresh == "invalidated" {
					InvalidateSQLiteTitles(store)
				}
				titles, _, err = AntigravityStoreTitles(t.Context(), store)
				require.NoError(t, err)
				assert.Empty(t, titles)
				assert.Empty(t, logs.String(), refresh)
			}

			require.NoError(t, os.Chtimes(store, now, now))
			titles, changed, err := AntigravityStoreTitles(t.Context(), store)
			require.NoError(t, err)
			require.NotEqual(t, state, changed)
			assert.Empty(t, titles)
			assert.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("Warning:")))
			assert.Contains(t, logs.String(), "missing column "+column)
		})
	}
}
