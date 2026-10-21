package service

import (
	"reflect"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/sandbox"
)

func launchProperties(p LaunchProfile, key id.ID) map[string]any {
	layout, _ := containment.ForJob(key)
	props := map[string]any{"Id": layout.Unit, "LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "Following": "", "Transient": false, "NeedDaemonReload": false, "RefuseManualStart": false, "FragmentPath": p.TemplatePath, "SourcePath": "", "ControlGroup": "", "DropInPaths": []string{}, "Job": []any{uint32(0), dbus.ObjectPath("/")}, "Type": "exec", "User": "root", "Group": "root", "Slice": "system.slice", "Delegate": true, "DelegateControllers": []string{"pids", "memory", "cpu"}, "DelegateSubgroup": "supervisor", "Restart": "no", "KillMode": "control-group", "KillSignal": int32(9), "SendSIGKILL": true, "TimeoutStartUSec": uint64(10 * time.Second / time.Microsecond), "TimeoutStopUSec": uint64(5 * time.Second / time.Microsecond), "RuntimeMaxUSec": uint64(p.WallTime / time.Microsecond), "OOMPolicy": "kill", "MemoryMax": p.MemoryBytes, "MemorySwapMax": uint64(0), "TasksMax": p.Tasks, "CPUQuotaPerSecUSec": p.CPUMillis * 1000, "UMask": uint32(0o077), "NoNewPrivileges": true, "LimitCORE": uint64(0), "StandardOutput": "null", "StandardError": "null", "Environment": []string{"LANG=C.UTF-8"}, "EnvironmentFiles": []any{}, "PassEnvironment": []string{}, "LoadCredential": [][]any{{"cortex-claim", p.StateDirectory + "/launch/" + key.String() + "/claim.json"}}, "ExecStart": [][]any{{p.Worker.Path, []string{p.Worker.Path, "--job", key.String(), "--config", p.Config.Path}, false, uint64(0), uint64(0), uint64(0), uint64(0), uint32(0), int32(0), int32(0)}}}
	for _, name := range []string{"ExecCondition", "ExecStartPre", "ExecStartPost", "ExecReload", "ExecStop", "ExecStopPost"} {
		props[name] = []any{}
	}
	for _, name := range []string{"BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories", "SetCredential", "SetCredentialEncrypted", "LoadCredentialEncrypted", "ImportCredential"} {
		props[name] = []any{}
	}
	for _, name := range []string{"RootDirectory", "RootImage", "WorkingDirectory"} {
		props[name] = ""
	}
	for _, name := range []string{"MainPID", "ExecMainPID", "ControlPID", "NRestarts"} {
		props[name] = uint32(0)
	}
	for _, name := range []string{"ExecMainStartTimestamp", "ExecMainStartTimestampMonotonic", "ExecMainExitTimestamp", "ExecMainExitTimestampMonotonic"} {
		props[name] = uint64(0)
	}
	props["ExecMainCode"], props["ExecMainStatus"] = int32(0), int32(0)
	props["PrivateMounts"] = true
	return props
}

func TestLaunchAdmissionRejectsEffectiveOverrides(t *testing.T) {
	p := profileFixture(t)
	key := id.New(sandbox.PrefixJob)
	props := launchProperties(p, key)
	if err := checkProperties(p, key, props); err != nil {
		t.Fatal(err)
	}
	for name, value := range props {
		t.Run(name, func(t *testing.T) {
			changed := launchProperties(p, key)
			delete(changed, name)
			if err := checkProperties(p, key, changed); err == nil {
				t.Fatal("missing effective setting admitted")
			}
			if reflect.TypeOf(value).Kind() == reflect.Slice {
				changed[name] = []any{"unexpected"}
			} else {
				changed[name] = "unexpected"
			}
			if err := checkProperties(p, key, changed); err == nil {
				t.Fatal("changed effective setting admitted")
			}
		})
	}
}

func TestLaunchAdmissionRejectsExtraCommandsAndForeignCredential(t *testing.T) {
	p := profileFixture(t)
	key := id.New(sandbox.PrefixJob)
	for _, name := range []string{"ExecStart", "ExecStartPre", "LoadCredential"} {
		props := launchProperties(p, key)
		switch rows := props[name].(type) {
		case [][]any:
			props[name] = append(rows, []any{"/evil"})
		case []any:
			props[name] = append(rows, []any{"/evil"})
		}
		if err := checkProperties(p, key, props); err == nil {
			t.Fatalf("extra %s admitted", name)
		}
	}
}
