package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestReinstallSystemdServiceInheritsTelemetryFromExistingUnitWhenEnvUnset is
// the telemetry counterpart of the proxy drift no-op guard (#610). After an
// install with NO_MISTAKES_TELEMETRY=0 exported, the on-disk unit carries the
// opt-out Environment= line. A later `daemon start` from a shell that does NOT
// export the variable must not re-detect drift and reinstall: doing so would
// strip the baked-in opt-out and silently re-enable daemon telemetry.
func TestReinstallSystemdServiceInheritsTelemetryFromExistingUnitWhenEnvUnset(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "linux"
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceExecutablePath = func() (string, error) { return "/usr/local/bin/no-mistakes", nil }

	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}

	// Render and persist the exact unit an opt-out-time install produces.
	for _, key := range forwardedEnvKeys() {
		t.Setenv(key, "")
	}
	t.Setenv("NO_MISTAKES_TELEMETRY", "0")
	unit := renderSystemdUnit("/usr/local/bin/no-mistakes", p, home)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}

	// The daemon now restarts from a shell WITHOUT the opt-out exported.
	for _, key := range forwardedEnvKeys() {
		t.Setenv(key, "")
	}

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return true, nil }

	changed, err := reinstallManagedServiceIfChanged(p)
	if err != nil {
		t.Fatalf("reinstallManagedServiceIfChanged: %v", err)
	}
	if changed {
		t.Fatal("env-less restart re-detected drift and reinstalled, which strips the baked-in telemetry opt-out")
	}
	if len(commands) != 0 {
		t.Fatalf("no systemctl command should run when there is no drift; ran %v", commands)
	}
	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != unit {
		t.Fatalf("unit changed after env-less restart:\n%s", data)
	}
	if !strings.Contains(string(data), `Environment="NO_MISTAKES_TELEMETRY=0"`) {
		t.Fatal("forwarded telemetry opt-out was stripped from the unit on env-less restart")
	}
}

// TestReinstallLaunchAgentInheritsTelemetryFromExistingPlistWhenEnvUnset is the
// launchd counterpart of the drift no-op guard.
func TestReinstallLaunchAgentInheritsTelemetryFromExistingPlistWhenEnvUnset(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "darwin"
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceExecutablePath = func() (string, error) { return "/usr/local/bin/no-mistakes", nil }

	plistPath := launchAgentPath(p)
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, key := range forwardedEnvKeys() {
		t.Setenv(key, "")
	}
	t.Setenv("NO_MISTAKES_TELEMETRY", "0")
	plist := renderLaunchAgent("/usr/local/bin/no-mistakes", p, home)
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, key := range forwardedEnvKeys() {
		t.Setenv(key, "")
	}

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return true, nil }

	changed, err := reinstallManagedServiceIfChanged(p)
	if err != nil {
		t.Fatalf("reinstallManagedServiceIfChanged: %v", err)
	}
	if changed {
		t.Fatal("env-less restart re-detected drift and reinstalled, which strips the baked-in telemetry opt-out")
	}
	if len(commands) != 0 {
		t.Fatalf("no launchctl command should run when there is no drift; ran %v", commands)
	}
	data, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != plist {
		t.Fatalf("plist changed after env-less restart:\n%s", data)
	}
	if !strings.Contains(string(data), "<key>NO_MISTAKES_TELEMETRY</key>") {
		t.Fatal("forwarded telemetry opt-out was stripped from the plist on env-less restart")
	}
}

// TestInstallSystemdUserServiceInheritsTelemetryFromExistingWhenEnvUnset guards
// the write path: when a reinstall is legitimately triggered (e.g. the binary
// path changed, or `no-mistakes update` re-renders the service) from a shell
// without the opt-out exported, the freshly written unit must still carry the
// telemetry values inherited from the prior on-disk unit rather than silently
// re-enabling daemon telemetry - and, with no proxy in play, keep the
// conventional 0644 mode because telemetry keys hold no credential.
func TestInstallSystemdUserServiceInheritsTelemetryFromExistingWhenEnvUnset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes (0644/0600) are not enforced on Windows; the forwarded-env service file is only generated on macOS/Linux")
	}
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "linux"
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceCommandRunner = func(string, ...string) ([]byte, error) { return nil, nil }

	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, key := range forwardedEnvKeys() {
		t.Setenv(key, "")
	}
	t.Setenv("NO_MISTAKES_TELEMETRY", "0")
	if err := os.WriteFile(unitPath, []byte(renderSystemdUnit("/old/no-mistakes", p, home)), 0o644); err != nil {
		t.Fatal(err)
	}

	// Reinstall from a shell without the opt-out exported.
	for _, key := range forwardedEnvKeys() {
		t.Setenv(key, "")
	}
	if err := installSystemdUserService(p, "/usr/local/bin/no-mistakes"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `Environment="NO_MISTAKES_TELEMETRY=0"`) {
		t.Fatalf("reinstall stripped the inherited telemetry opt-out:\n%s", data)
	}
	if !strings.Contains(string(data), "ExecStart=/usr/local/bin/no-mistakes") {
		t.Fatalf("reinstall should update the executable:\n%s", data)
	}
	if info, err := os.Stat(unitPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o644 {
		t.Fatalf("telemetry-only unit mode = %o, want 0644 (telemetry keys carry no credential)", info.Mode().Perm())
	}
}

// TestInstallSystemdUserServiceKeepsBakedProxyWhenOnlyTelemetryExported guards
// the per-category inheritance in mergeForwardedEnv: a reinstall from a shell
// that exports only NO_MISTAKES_TELEMETRY must not treat "some forwarded env is
// present" as "replace everything" and strip the proxy already baked into the
// on-disk unit - the daemon would silently lose network access through the
// user's proxy. The proxy is inherited, the telemetry value comes from the live
// environment, and the proxy keeps the unit at owner-only 0600.
func TestInstallSystemdUserServiceKeepsBakedProxyWhenOnlyTelemetryExported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes (0644/0600) are not enforced on Windows; the forwarded-env service file is only generated on macOS/Linux")
	}
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "linux"
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceCommandRunner = func(string, ...string) ([]byte, error) { return nil, nil }

	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, key := range forwardedEnvKeys() {
		t.Setenv(key, "")
	}
	t.Setenv("HTTPS_PROXY", "http://user:pass@127.0.0.1:7897")
	if err := os.WriteFile(unitPath, []byte(renderSystemdUnit("/usr/local/bin/no-mistakes", p, home)), 0o600); err != nil {
		t.Fatal(err)
	}

	// Reinstall from a shell exporting only the telemetry opt-out.
	for _, key := range forwardedEnvKeys() {
		t.Setenv(key, "")
	}
	t.Setenv("NO_MISTAKES_TELEMETRY", "0")
	if err := installSystemdUserService(p, "/usr/local/bin/no-mistakes"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `Environment="HTTPS_PROXY=http://user:pass@127.0.0.1:7897"`) {
		t.Fatalf("telemetry-exporting reinstall stripped the baked-in proxy:\n%s", data)
	}
	if !strings.Contains(string(data), `Environment="NO_MISTAKES_TELEMETRY=0"`) {
		t.Fatalf("reinstall should bake the live telemetry opt-out:\n%s", data)
	}
	if info, err := os.Stat(unitPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("proxy-bearing unit mode = %o, want 0600", info.Mode().Perm())
	}
}
