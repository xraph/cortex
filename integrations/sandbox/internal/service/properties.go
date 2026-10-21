package service

import (
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
)

func checkProperties(p LaunchProfile, key id.ID, properties map[string]any) error {
	if err := p.validate(); err != nil {
		return err
	}
	layout, err := containment.ForJob(key)
	if err != nil {
		return err
	}
	wallMicros := p.WallTime.Microseconds()
	if wallMicros <= 0 {
		return fmt.Errorf("cortex: invalid service wall clock bound")
	}
	expected := map[string]any{
		"Id": layout.Unit, "LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "Following": "", "Transient": false, "NeedDaemonReload": false, "RefuseManualStart": false, "FragmentPath": p.TemplatePath, "SourcePath": "", "ControlGroup": "", "RootDirectory": "", "RootImage": "", "WorkingDirectory": "",
		"Type": "exec", "User": "root", "Group": "root", "Slice": "system.slice", "Delegate": true, "DelegateSubgroup": "supervisor", "Restart": "no", "KillMode": "control-group", "KillSignal": int32(9), "SendSIGKILL": true,
		"TimeoutStartUSec": uint64(10 * time.Second / time.Microsecond), "TimeoutStopUSec": uint64(5 * time.Second / time.Microsecond), "RuntimeMaxUSec": uint64(wallMicros), "OOMPolicy": "kill", "MemoryMax": p.MemoryBytes, "MemorySwapMax": uint64(0), "TasksMax": p.Tasks, "CPUQuotaPerSecUSec": p.CPUMillis * 1000,
		"UMask": uint32(0o077), "NoNewPrivileges": true, "LimitCORE": uint64(0), "StandardOutput": "null", "StandardError": "null", "Environment": []string{"LANG=C.UTF-8"},
		"Job":            []any{uint32(0), dbus.ObjectPath("/")},
		"LoadCredential": [][]any{{"cortex-claim", p.StateDirectory + "/launch/" + key.String() + "/claim.json"}},
		"ExecStart":      [][]any{{p.Worker.Path, []string{p.Worker.Path, "--job", key.String(), "--config", p.Config.Path}, false, uint64(0), uint64(0), uint64(0), uint64(0), uint32(0), int32(0), int32(0)}},
	}
	for name, value := range expected {
		if !reflect.DeepEqual(properties[name], value) {
			return fmt.Errorf("cortex: effective worker setting %s differs from approved profile", name)
		}
	}
	for _, name := range []string{"DropInPaths", "EnvironmentFiles", "PassEnvironment", "ExecCondition", "ExecStartPre", "ExecStartPost", "ExecReload", "ExecStop", "ExecStopPost", "BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories", "SetCredential", "SetCredentialEncrypted", "LoadCredentialEncrypted", "ImportCredential"} {
		value, ok := properties[name]
		if !ok || value == nil {
			return fmt.Errorf("cortex: missing worker setting %s", name)
		}
		list := reflect.ValueOf(value)
		if list.Kind() != reflect.Slice || list.Len() != 0 {
			return fmt.Errorf("cortex: unexpected worker override %s", name)
		}
	}
	controllers, ok := properties["DelegateControllers"].([]string)
	if !ok {
		return fmt.Errorf("cortex: missing delegated worker controllers")
	}
	controllers = slices.Clone(controllers)
	slices.Sort(controllers)
	if !slices.Equal(controllers, []string{"cpu", "memory", "pids"}) {
		return fmt.Errorf("cortex: unexpected worker controller delegation")
	}
	return nil
}
