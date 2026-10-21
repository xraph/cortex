package service

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

const templateName = "cortex-sandbox-worker@.service"

// LaunchProfile pins host deployment and outer service ceilings. These limits
// include worker/runtime overhead and are separate from each guest's limits.
// WallTime is an outer ceiling, not the receipt's absolute acceptance deadline.
type LaunchProfile struct {
	SchemaVersion                int
	Worker, Config               hostfs.Pin
	TemplatePath, StateDirectory string
	CPUMillis, MemoryBytes       uint64
	Tasks                        uint64
	WallTime                     time.Duration
}

// Digest binds the approved host paths, file pins and outer service ceilings.
func (p LaunchProfile) Digest() (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return evidence.Digest(body), nil
}

func (p LaunchProfile) validate() error {
	if p.SchemaVersion != 1 || !profilePath(p.TemplatePath) || path.Base(p.TemplatePath) != templateName || !profilePath(p.StateDirectory) || p.CPUMillis <= 0 || p.CPUMillis > 1_000_000 || p.MemoryBytes <= 0 || p.MemoryBytes > 1<<40 || p.Tasks == 0 || p.Tasks > 65536 || p.WallTime < time.Second || p.WallTime > 24*time.Hour || p.WallTime%time.Microsecond != 0 {
		return fmt.Errorf("cortex: invalid worker service profile")
	}
	for _, pin := range []hostfs.Pin{p.Worker, p.Config} {
		if !profilePath(pin.Path) || !evidence.ValidDigest(pin.SHA256) || pin.Bytes <= 0 || pin.Bytes > 2<<30 || under(pin.Path, p.StateDirectory) {
			return fmt.Errorf("cortex: unsafe worker deployment pin")
		}
	}
	if p.Worker.Mode != 0o755 || p.Config.Mode != 0o600 || p.Config.Bytes > 1<<20 || p.Worker.Path == p.Config.Path || p.Worker.Path == p.TemplatePath || p.Config.Path == p.TemplatePath || under(p.TemplatePath, p.StateDirectory) {
		return fmt.Errorf("cortex: overlapping or writable worker deployment")
	}
	return nil
}

func under(name, root string) bool { return name == root || strings.HasPrefix(name, root+"/") }
func profilePath(name string) bool {
	if len(name) > 1024 || name == "/" || !strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return false
	}
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("/._-@", c) {
			continue
		}
		return false
	}
	return true
}

// Template renders the one supported installed service template. Paths cannot
// inject directives, substitutions or shell syntax. Installation is a separate
// host operation; this function neither writes unit files nor starts workers.
func (p LaunchProfile) Template() ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	body := fmt.Sprintf(`[Unit]
Description=Cortex sandbox worker

[Service]
Type=exec
User=root
Group=root
Slice=system.slice
Delegate=cpu memory pids
DelegateSubgroup=supervisor
Restart=no
KillMode=control-group
KillSignal=SIGKILL
SendSIGKILL=yes
TimeoutStartSec=10s
TimeoutStopSec=5s
RuntimeMaxSec=%dus
OOMPolicy=kill
MemoryMax=%d
MemorySwapMax=0
TasksMax=%d
CPUQuota=%d.%d%%
UMask=0077
NoNewPrivileges=yes
LimitCORE=0
StandardOutput=null
StandardError=null
Environment=LANG=C.UTF-8
LoadCredential=cortex-claim:%s/launch/%%i/claim.json
ExecStart=%s --job %%i --config %s
`, p.WallTime/time.Microsecond, p.MemoryBytes, p.Tasks, p.CPUMillis/10, p.CPUMillis%10, p.StateDirectory, p.Worker.Path, p.Config.Path)
	return []byte(body), nil
}
