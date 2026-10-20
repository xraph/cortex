//go:build linux || darwin

package sandboxbroker_test

import (
	"os"
	"path/filepath"
	"testing"

	broker "github.com/xraph/cortex/integrations/sandbox"
)

func TestArtifactExportRejectsTraversalAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "result"), []byte("output"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("result", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested", filepath.Join(dir, "dirlink")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../result", "/result", "link", "dirlink/result", "nested/../result", "."} {
		if _, err := broker.ReadExport(dir, name, 128); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	data, err := broker.ReadExport(dir, "result", 128)
	if err != nil || string(data) != "output" {
		t.Fatalf("%q %v", data, err)
	}
	if _, err = broker.ReadExport(dir, "result", 2); err == nil {
		t.Fatal("byte limit ignored")
	}
}
