package deployment_test

import (
	"strings"
	"testing"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/deployment"
)

func fixtureManifest() deployment.Manifest {
	return deployment.Manifest{SchemaVersion: 1, Architecture: "amd64", Files: []deployment.File{
		{Path: "runsc", SHA256: evidence.Digest([]byte("runtime")), Bytes: 7, Mode: 0o755},
		{Path: "gvisor-bin/sentry", SHA256: evidence.Digest([]byte("sidecar")), Bytes: 7, Mode: 0o755},
	}}
}

func TestManifestPinsCompleteCanonicalInventory(t *testing.T) {
	m := fixtureManifest()
	digest, err := m.Digest()
	if err != nil || !evidence.ValidDigest(digest) {
		t.Fatalf("digest: %s %v", digest, err)
	}
	m.Files[0], m.Files[1] = m.Files[1], m.Files[0]
	reordered, err := m.Digest()
	if err != nil || reordered != digest {
		t.Fatal("inventory order changes deployment identity")
	}
	if m.Files[0].Path != "gvisor-bin/sentry" {
		t.Fatal("digest mutated caller inventory")
	}
	m.Files[0].SHA256 = strings.Repeat("a", 64)
	changed, err := m.Digest()
	if err != nil || changed == digest {
		t.Fatal("sidecar substitution not bound")
	}
	m = fixtureManifest()
	m.Architecture = "arm64"
	changed, err = m.Digest()
	if err != nil || changed == digest {
		t.Fatal("architecture not bound")
	}
}

func TestManifestRejectsAmbiguousOrUnboundedInventory(t *testing.T) {
	cases := map[string]func(*deployment.Manifest){
		"version":       func(m *deployment.Manifest) { m.SchemaVersion = 0 },
		"architecture":  func(m *deployment.Manifest) { m.Architecture = "native" },
		"missing runsc": func(m *deployment.Manifest) { m.Files = m.Files[1:] },
		"duplicate":     func(m *deployment.Manifest) { m.Files = append(m.Files, m.Files[0]) },
		"absolute":      func(m *deployment.Manifest) { m.Files[1].Path = "/gvisor-bin/sentry" },
		"traversal":     func(m *deployment.Manifest) { m.Files[1].Path = "gvisor-bin/../sentry" },
		"unrelated":     func(m *deployment.Manifest) { m.Files[1].Path = "bin/sentry" },
		"empty":         func(m *deployment.Manifest) { m.Files[1].Bytes = 0 },
		"oversize":      func(m *deployment.Manifest) { m.Files[1].Bytes = 2<<30 + 1 },
		"digest":        func(m *deployment.Manifest) { m.Files[1].SHA256 = "latest" },
		"setuid":        func(m *deployment.Manifest) { m.Files[1].Mode = 0o4755 },
		"writable":      func(m *deployment.Manifest) { m.Files[1].Mode = 0o777 },
		"private":       func(m *deployment.Manifest) { m.Files[1].Mode = 0o700 },
		"file directory collision": func(m *deployment.Manifest) {
			m.Files = append(m.Files, deployment.File{Path: "gvisor-bin/sentry/child", SHA256: m.Files[0].SHA256, Bytes: 7, Mode: 0o755})
		},
		"inventory limit": func(m *deployment.Manifest) {
			m.Files = make([]deployment.File, 129)
		},
		"total byte limit": func(m *deployment.Manifest) {
			m.Files[0].Bytes = 2 << 30
			m.Files[1].Bytes = 2 << 30
			m.Files = append(m.Files, deployment.File{Path: "gvisor-bin/extra", SHA256: m.Files[0].SHA256, Bytes: 1, Mode: 0o755})
		},
		"depth limit": func(m *deployment.Manifest) {
			m.Files[1].Path = "gvisor-bin/" + strings.Repeat("nested/", 8) + "sentry"
		},
		"invalid text": func(m *deployment.Manifest) { m.Files[1].Path = "gvisor-bin/\xff" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := fixtureManifest()
			mutate(&m)
			if _, err := m.Digest(); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}
