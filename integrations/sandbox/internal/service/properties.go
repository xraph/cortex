package service

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
)

var errWorkerActivating = errors.New("cortex: worker activation pending")

func checkWorkerProperties(p LaunchProfile, key id.ID, properties map[string]any, pid uint32) error {
	if pid <= 1 {
		return fmt.Errorf("cortex: invalid worker main process")
	}
	return checkEffectiveProperties(p, key, properties, pid)
}

func checkProperties(p LaunchProfile, key id.ID, properties map[string]any) error {
	return checkEffectiveProperties(p, key, properties, 0)
}

func checkEffectiveProperties(p LaunchProfile, key id.ID, properties map[string]any, workerPID uint32) error {
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
	command := [][]any{{p.Worker.Path, []string{p.Worker.Path, "--job", key.String(), "--config", p.Config.Path}, false, uint64(0), uint64(0), uint64(0), uint64(0), uint32(0), int32(0), int32(0)}}
	expected := map[string]any{
		"PrivateMounts": true,
		"Id":            layout.Unit, "LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "Following": "", "Transient": false, "NeedDaemonReload": false, "RefuseManualStart": false, "FragmentPath": p.TemplatePath, "SourcePath": "", "ControlGroup": "", "RootDirectory": "", "RootImage": "", "WorkingDirectory": "",
		"Type": "exec", "User": "root", "Group": "root", "Slice": "system.slice", "Delegate": true, "DelegateSubgroup": "supervisor", "Restart": "no", "KillMode": "control-group", "KillSignal": int32(9), "SendSIGKILL": true,
		"TimeoutStartUSec": uint64(10 * time.Second / time.Microsecond), "TimeoutStopUSec": uint64(5 * time.Second / time.Microsecond), "RuntimeMaxUSec": uint64(wallMicros), "OOMPolicy": "kill", "MemoryMax": p.MemoryBytes, "MemorySwapMax": uint64(0), "TasksMax": p.Tasks, "CPUQuotaPerSecUSec": p.CPUMillis * 1000,
		"UMask": uint32(0o077), "NoNewPrivileges": true, "LimitCORE": uint64(0), "StandardOutput": "null", "StandardError": "null", "Environment": []string{"LANG=C.UTF-8"},
		"Job":            []any{uint32(0), dbus.ObjectPath("/")},
		"LoadCredential": [][]any{{"cortex-claim", p.StateDirectory + "/launch/" + key.String() + "/claim.json"}},
		"ExecStart":      command,
		"MainPID":        uint32(0), "ExecMainPID": uint32(0), "ControlPID": uint32(0), "NRestarts": uint32(0), "ExecMainStartTimestamp": uint64(0), "ExecMainStartTimestampMonotonic": uint64(0), "ExecMainExitTimestamp": uint64(0), "ExecMainExitTimestampMonotonic": uint64(0), "ExecMainCode": int32(0), "ExecMainStatus": int32(0),
	}
	pending := false
	if workerPID != 0 {
		expected["ActiveState"], expected["SubState"], expected["ControlGroup"] = "active", "running", layout.Parent
		if job, ok := properties["Job"].([]any); ok && len(job) == 2 {
			if jobID, valid := job[0].(uint32); valid && jobID != 0 && job[1] == dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/systemd1/job/%d", jobID)) {
				expected["Job"] = job
				pending = true
				if properties["ActiveState"] == "activating" && properties["SubState"] == "start" {
					expected["ActiveState"], expected["SubState"] = "activating", "start"
				}
			}
		}
		expected["MainPID"], expected["ExecMainPID"] = workerPID, workerPID
		for _, name := range []string{"ExecMainStartTimestamp", "ExecMainStartTimestampMonotonic"} {
			stamp, ok := properties[name].(uint64)
			if !ok || stamp == 0 {
				return fmt.Errorf("cortex: running worker has no start timestamp")
			}
			expected[name] = stamp
		}
		rows, ok := properties["ExecStart"].([][]any)
		if !ok || len(rows) != 1 || len(rows[0]) != 10 {
			return fmt.Errorf("cortex: malformed worker execution record")
		}
		for _, index := range []int{3, 4} {
			stamp, valid := rows[0][index].(uint64)
			if !valid || stamp == 0 {
				return fmt.Errorf("cortex: worker command has no start timestamp")
			}
			command[0][index] = stamp
		}
		command[0][7] = workerPID
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
	if pending {
		return errWorkerActivating
	}
	return nil
}
