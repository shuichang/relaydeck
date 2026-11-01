package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
)

func TestMain(m *testing.M) {
	if os.Getenv("RELAYDECK_ENGINE_HELPER") == "1" {
		os.Exit(runEngineHelper(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runEngineHelper(args []string) int {
	recordHelperInvocation(args)
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Println("sing-box version 1.14.4")
		return 0
	case "check":
		if len(args) != 3 || args[1] != "-c" {
			fmt.Fprintln(os.Stderr, "unexpected check arguments")
			return 2
		}
		content, flags := readHelperConfig(args[2])
		if _, err := os.Stat(args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if flags["reject_check"] == true {
			fmt.Fprintf(os.Stderr, "password=%s\n", helperString(content, "password"))
			return 1
		}
		return 0
	case "run":
		if len(args) != 3 || args[1] != "-c" {
			fmt.Fprintln(os.Stderr, "unexpected run arguments")
			return 2
		}
		content, flags := readHelperConfig(args[2])
		if flags["fail_start"] == true {
			fmt.Fprintf(os.Stderr, "password=%s\n", helperString(content, "password"))
			return 1
		}
		if flags["crash_once"] == true {
			statePath := os.Getenv("RELAYDECK_ENGINE_HELPER_STATE")
			if _, err := os.Stat(statePath); os.IsNotExist(err) {
				_ = os.WriteFile(statePath, []byte("crashed\n"), 0600)
				time.Sleep(800 * time.Millisecond)
				return 3
			}
		}
		if flags["emit_secret"] == true {
			fmt.Fprintf(os.Stdout, "password=%s\n", helperString(content, "password"))
		}
		for {
			time.Sleep(time.Hour)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown helper action")
		return 2
	}
}

func recordHelperInvocation(args []string) {
	path := os.Getenv("RELAYDECK_ENGINE_RECORD")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(file, strings.Join(args, "\x1f"))
	_ = file.Close()
}

func readHelperConfig(path string) (map[string]any, map[string]bool) {
	content := map[string]any{}
	flags := map[string]bool{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return content, flags
	}
	_ = json.Unmarshal(raw, &content)
	for _, key := range []string{"emit_secret", "reject_check", "fail_start"} {
		flags[key], _ = content[key].(bool)
	}
	return content, flags
}

func helperString(content map[string]any, key string) string {
	value, _ := content[key].(string)
	return value
}

func helperManager(t *testing.T, dir string) (*Manager, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "invocations.log")
	t.Setenv("RELAYDECK_ENGINE_HELPER", "1")
	t.Setenv("RELAYDECK_ENGINE_RECORD", record)
	t.Setenv("RELAYDECK_ENGINE_HELPER_STATE", filepath.Join(dir, "helper-state"))
	manager := New(filepath.Join(dir, "data"), executable)
	t.Cleanup(manager.Close)
	return manager, record
}

func TestManagerLifecycleAndRedaction(t *testing.T) {
	dir := t.TempDir()
	manager, record := helperManager(t, dir)

	engineConfig := json.RawMessage(`{"password":"super-secret-123","emit_secret":true}`)
	if err := manager.Apply(config.Engine{Enabled: true, Config: engineConfig}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot := manager.Snapshot()
		if snapshot.Running && strings.Contains(strings.Join(snapshot.Logs, "\n"), "[REDACTED]") {
			if snapshot.Available != true || !strings.Contains(snapshot.Version, "1.14.4") {
				t.Fatalf("unexpected binary state: %+v", snapshot)
			}
			if snapshot.Enabled != true || snapshot.Error != "" {
				t.Fatalf("unexpected engine state: %+v", snapshot)
			}
			if strings.Contains(strings.Join(snapshot.Logs, "\n"), "super-secret-123") {
				t.Fatal("credential leaked into logs")
			}
			snapshot.Config[0] = '['
			if manager.Snapshot().Config[0] != '{' {
				t.Fatal("Snapshot returned shared config storage")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine did not report running and redacted log output: %+v", manager.Snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}

	manager.mu.Lock()
	configPath := manager.configPath
	manager.mu.Unlock()
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("config mode = %#o, want 0600", info.Mode().Perm())
	}

	invocations, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(invocations)), "\n")
	if len(lines) < 3 {
		t.Fatalf("missing helper invocations: %q", invocations)
	}
	if lines[0] != "version" || !strings.HasPrefix(lines[1], "check\x1f-c\x1f") || !strings.HasPrefix(lines[2], "run\x1f-c\x1f") {
		t.Fatalf("unexpected invocation sequence: %q", lines)
	}

	manager.Close()
	if manager.Snapshot().Running {
		t.Fatal("manager still reports running after Close")
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("runtime config was not removed: %v", err)
	}
}

func TestCheckFailureKeepsPreviousStateAndRedacts(t *testing.T) {
	dir := t.TempDir()
	manager, _ := helperManager(t, dir)
	old := json.RawMessage(`{"password":"old-secret","enabled_marker":true}`)
	if err := manager.Apply(config.Engine{Enabled: true, Config: old}); err != nil {
		t.Fatal(err)
	}

	bad := json.RawMessage(`{"password":"new-super-secret","reject_check":true}`)
	err := manager.Apply(config.Engine{Enabled: true, Config: bad})
	if err == nil {
		t.Fatal("invalid config unexpectedly applied")
	}
	if strings.Contains(err.Error(), "new-super-secret") {
		t.Fatalf("credential leaked into returned error: %v", err)
	}
	snapshot := manager.Snapshot()
	if !snapshot.Running || !bytesEqualJSON(snapshot.Config, old) {
		t.Fatalf("previous state was not preserved: %+v", snapshot)
	}
	if strings.Contains(strings.Join(snapshot.Logs, "\n"), "new-super-secret") {
		t.Fatal("credential leaked into logs")
	}
}

func TestStartupFailureRestoresPreviousProcessAndConfig(t *testing.T) {
	dir := t.TempDir()
	manager, _ := helperManager(t, dir)
	old := json.RawMessage(`{"password":"old-secret","stable":true}`)
	if err := manager.Apply(config.Engine{Enabled: true, Config: old}); err != nil {
		t.Fatal(err)
	}

	bad := json.RawMessage(`{"password":"new-start-secret","fail_start":true}`)
	err := manager.Apply(config.Engine{Enabled: true, Config: bad})
	if err == nil {
		t.Fatal("startup failure unexpectedly applied")
	}
	if strings.Contains(err.Error(), "new-start-secret") {
		t.Fatalf("credential leaked into returned error: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot := manager.Snapshot()
		if snapshot.Running && bytesEqualJSON(snapshot.Config, old) {
			if strings.Contains(snapshot.Error, "new-start-secret") || strings.Contains(strings.Join(snapshot.Logs, "\n"), "new-start-secret") {
				t.Fatal("credential leaked after rollback")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("previous process was not restored: %+v", snapshot)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestUnexpectedExitRestartsWithBackoff(t *testing.T) {
	dir := t.TempDir()
	manager, _ := helperManager(t, dir)
	engineConfig := json.RawMessage(`{"password":"restart-secret","crash_once":true}`)
	if err := manager.Apply(config.Engine{Enabled: true, Config: engineConfig}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(4 * time.Second)
	for {
		snapshot := manager.Snapshot()
		joinedLogs := strings.Join(snapshot.Logs, "\n")
		if strings.Contains(joinedLogs, "自动重启成功") {
			if !snapshot.Running || snapshot.Error != "" {
				t.Fatalf("restarted process has unexpected state: %+v", snapshot)
			}
			if strings.Contains(joinedLogs, "restart-secret") {
				t.Fatal("credential leaked during restart")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process did not recover after unexpected exit: %+v", snapshot)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLogRingIsBoundedAndRedacted(t *testing.T) {
	ring := newLogRing(5)
	ring.replaceSecrets(secretsFromConfig(json.RawMessage(`{"password":"private-password"}`)))
	for i := 0; i < 20; i++ {
		ring.append(-1, fmt.Sprintf("line-%d password=private-password", i))
	}
	ring.write(0, []byte(strings.Repeat("x", maxLogLineBytes*3)+"\n"))
	ring.flushPartial()

	logs := ring.snapshot()
	if len(logs) != 5 {
		t.Fatalf("got %d log lines, want 5", len(logs))
	}
	for _, line := range logs {
		if len(line) > maxLogLineBytes+len(" …[已截断]")+len("[stdout] ") {
			t.Fatalf("oversized log line: %d bytes", len(line))
		}
		if strings.Contains(line, "private-password") {
			t.Fatal("credential leaked into ring log")
		}
	}
	if !strings.Contains(logs[len(logs)-1], "已截断") {
		t.Fatalf("long line was not marked truncated: %q", logs[len(logs)-1])
	}
}

func TestTemplatesMatchCurrentShape(t *testing.T) {
	templates := Templates()
	if len(templates) < 9 {
		t.Fatalf("got %d templates", len(templates))
	}
	seen := make(map[string]bool)
	for _, template := range templates {
		if template.ID == "" || seen[template.ID] {
			t.Fatalf("empty or duplicate template ID %q", template.ID)
		}
		seen[template.ID] = true
		var configObject map[string]any
		if err := json.Unmarshal(template.Config, &configObject); err != nil {
			t.Fatalf("%s is invalid JSON: %v", template.ID, err)
		}
		inbounds, _ := configObject["inbounds"].([]any)
		if len(inbounds) != 1 {
			t.Fatalf("%s has unexpected inbounds", template.ID)
		}
		inbound, _ := inbounds[0].(map[string]any)
		if inbound["override_address"] != nil || inbound["override_port"] != nil {
			t.Fatalf("%s uses obsolete inbound destination override fields", template.ID)
		}
		route, _ := configObject["route"].(map[string]any)
		rules, _ := route["rules"].([]any)
		if len(rules) == 0 {
			t.Fatalf("%s has no route rule", template.ID)
		}
		rule, _ := rules[0].(map[string]any)
		if rule["action"] != "route" || rule["override_address"] == nil || rule["override_port"] == nil {
			t.Fatalf("%s does not map through the current route action", template.ID)
		}
	}

	first := Templates()
	first[0].Config[0] = '['
	if Templates()[0].Config[0] != '{' {
		t.Fatal("caller mutated the built-in template catalog")
	}
}

func TestTemplatesWithOfficialSingBox(t *testing.T) {
	binary := os.Getenv("SING_BOX_BINARY")
	if binary == "" {
		t.Skip("set SING_BOX_BINARY to run sing-box template validation")
	}
	dir := t.TempDir()
	for _, template := range Templates() {
		t.Run(template.ID, func(t *testing.T) {
			path := filepath.Join(dir, template.ID+".json")
			if err := os.WriteFile(path, template.Config, 0600); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command(binary, "check", "-c", path).CombinedOutput()
			if err != nil {
				t.Fatalf("sing-box check failed: %v\n%s", err, output)
			}
		})
	}
}

func TestMissingBinaryDoesNotBreakDisabledEngine(t *testing.T) {
	dir := t.TempDir()
	manager := New(filepath.Join(dir, "data"), filepath.Join(dir, "missing-sing-box"))
	t.Cleanup(manager.Close)

	if snapshot := manager.Snapshot(); snapshot.Available || snapshot.Running || snapshot.Error != "" {
		t.Fatalf("unexpected initial snapshot: %+v", snapshot)
	}
	if err := manager.Apply(config.Engine{Enabled: false, Config: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("disabled default engine should remain usable without sing-box: %v", err)
	}
	if err := manager.Apply(config.Engine{Enabled: true, Config: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("enabled engine unexpectedly reported success without a binary")
	}
	snapshot := manager.Snapshot()
	if snapshot.Enabled || snapshot.Running || snapshot.Available {
		t.Fatalf("missing binary changed process state: %+v", snapshot)
	}
}

func bytesEqualJSON(first, second []byte) bool {
	var a any
	var b any
	return json.Unmarshal(first, &a) == nil && json.Unmarshal(second, &b) == nil && reflect.DeepEqual(a, b)
}
