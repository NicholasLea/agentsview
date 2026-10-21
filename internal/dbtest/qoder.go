package dbtest

import (
	"testing"

	"go.kenn.io/agentsview/internal/testjsonl"
)

const QoderTitleID = testjsonl.QoderTitleID

// QoderTitleRoot returns the international session root that Qoder's
// application database titles, with that database path. Titles bind to a home
// directory rather than a test temporary one, so this fakes the caller's home;
// any other root, including a mirror extraction, resolves to no database.
func QoderTitleRoot(t *testing.T) (root, store string) {
	t.Helper()
	return testjsonl.QoderTitleRoot(t)
}

func WriteQoderTitleTranscript(t *testing.T, root, content string) string {
	t.Helper()
	return testjsonl.WriteQoderTranscript(t, root, QoderTitleID, content)
}

// SetQoderTitle writes a title into the application database and returns its
// path, so a caller can pass it as a changed path.
func SetQoderTitle(t *testing.T, store string, title any) string {
	t.Helper()
	testjsonl.SetQoderTitle(t, store, QoderTitleID, title)
	return store
}
