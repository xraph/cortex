package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestControlSocketRequiresPrivateCanonicalEndpoint(t *testing.T) {
	_, path := controlListener(t)
	owner := uint32(os.Getuid())
	if err := inspectControlSocket(path, owner); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := inspectControlSocket(path, owner); err == nil {
		t.Fatal("public socket accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(path)
	if err := os.Chmod(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := inspectControlSocket(path, owner); err == nil {
		t.Fatal("non-private parent accepted")
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "alias.sock")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := inspectControlSocket(link, owner); err == nil {
		t.Fatal("socket alias accepted")
	}
	regular := filepath.Join(parent, "regular")
	if err := os.WriteFile(regular, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := inspectControlSocket(regular, owner); err == nil {
		t.Fatal("regular file accepted")
	}
	for _, bad := range []string{"relative.sock", parent + "/../" + filepath.Base(parent) + "/control.sock", "@abstract", path + "\x00"} {
		if err := inspectControlSocket(bad, owner); err == nil {
			t.Fatal("noncanonical endpoint accepted")
		}
	}
}
