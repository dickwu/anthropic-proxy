package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInspectorRejectsNonPrivateExistingDirectoryWithoutChangingIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	listener, err := startInspector(filepath.Join(dir, "proxy.sock"), &snapshotStore{})
	if err == nil {
		listener.Close()
		t.Fatal("nonprivate existing directory accepted")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("existing directory mode changed to %o", info.Mode().Perm())
	}
}
