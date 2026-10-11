package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// linkQoderNativeRoot links either the client directory or its projects
// directory under home to a directory outside it, and returns the native
// root as spelled under home. The test is skipped where symlinks cannot be
// created.
func linkQoderNativeRoot(t *testing.T, home, clientDir, position string) string {
	t.Helper()
	outside := filepath.Join(t.TempDir(), "physical")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	native := filepath.Join(home, clientDir)
	link := native
	if position == "projects directory" {
		require.NoError(t, os.MkdirAll(native, 0o755))
		link = filepath.Join(native, "projects")
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return filepath.Join(native, "projects")
}

// TestQoderParseReadsTitleThroughSymlinkedNativeRoot covers a native client
// tree linked outside the home directory. Configuration loading resolves the
// transcript root to its physical destination, and the parse must still bind
// the client's application database instead of treating the root as custom.
func TestQoderParseReadsTitleThroughSymlinkedNativeRoot(t *testing.T) {
	for _, client := range []struct{ dir, app string }{
		{".qoder", qoderIntlClientApp}, {".qoder-cn", qoderCNClientApp},
	} {
		for _, position := range []string{"client directory", "projects directory"} {
			t.Run(client.dir+"/"+position, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("USERPROFILE", home)
				native := linkQoderNativeRoot(t, home, client.dir, position)
				root, _, err := ResolveProviderRoot(AgentQoder, native)
				require.NoError(t, err)
				require.NotEqual(t, filepath.Clean(native), root,
					"the fixture must exercise a resolved root")
				writeQoderTitleTestSession(t, root, qoderTitleTestID)
				title := "linked native title"
				dbPath := filepath.Join(home, "Library", "Application Support", client.app, "main.sqlite")
				writeQoderTitleDB(t, dbPath, map[string]*string{qoderTitleTestID: &title})

				provider := qoderTitleTestProvider(t, []string{root})
				sess := qoderTitleTestParse(t, provider, root)[0].Result.Session
				assert.Equal(t, title, sess.SessionName)
				assert.True(t, sess.SessionNamePresent)
				watch, err := ResolveWatchRoots(t.Context(), provider)
				require.NoError(t, err)
				assert.True(t, watchRootContains(watch, filepath.Dir(dbPath)))
				if position == "client directory" {
					// The client directory itself is a supported root.
					clientRoot, _, err := ResolveProviderRoot(AgentQoder, filepath.Join(home, client.dir))
					require.NoError(t, err)
					assert.Equal(t, client.app, QoderLocalClientAppForPath(clientRoot))
				}
			})
		}
	}

	t.Run("linked custom root stays custom", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		outside := filepath.Join(t.TempDir(), ".qoder", "projects")
		require.NoError(t, os.MkdirAll(outside, 0o755))
		if err := os.Symlink(outside, filepath.Join(home, "custom")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		root, _, err := ResolveProviderRoot(AgentQoder, filepath.Join(home, "custom"))
		require.NoError(t, err)
		assert.Empty(t, QoderLocalClientAppForPath(root))
		assert.Empty(t, resolveQoderAppSQLitePath(filepath.Join(root, "proj", "s.jsonl"), []string{root}))
	})
}
