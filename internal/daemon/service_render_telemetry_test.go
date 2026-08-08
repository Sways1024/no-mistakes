package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestRenderLaunchAgentForwardsTelemetryOptOut is the launchd half of the #610
// fix: NO_MISTAKES_TELEMETRY=0 exported when the service is installed must be
// baked into the plist's EnvironmentVariables dict, because the managed daemon
// inherits its environment from launchd - not from the shell that ran
// `no-mistakes init` - and the opt-out check reads the process environment.
func TestRenderLaunchAgentForwardsTelemetryOptOut(t *testing.T) {
	for _, key := range forwardedEnvKeys() {
		t.Setenv(key, "")
	}
	t.Setenv("NO_MISTAKES_TELEMETRY", "0")

	plist := renderLaunchAgent("/opt/no-mistakes/bin/no-mistakes", paths.WithRoot(t.TempDir()), "/home/u")
	fragment := "<key>NO_MISTAKES_TELEMETRY</key>\n    <string>0</string>"
	if !strings.Contains(plist, fragment) {
		t.Fatalf("launch agent should forward the telemetry opt-out %q, got:\n%s", fragment, plist)
	}
}

// TestRenderSystemdUnitForwardsTelemetryOptOut is the systemd counterpart.
func TestRenderSystemdUnitForwardsTelemetryOptOut(t *testing.T) {
	for _, key := range forwardedEnvKeys() {
		t.Setenv(key, "")
	}
	t.Setenv("NO_MISTAKES_TELEMETRY", "0")

	unit := renderSystemdUnit("/usr/local/bin/no-mistakes", paths.WithRoot(t.TempDir()), "/home/u")
	want := `Environment="NO_MISTAKES_TELEMETRY=0"`
	if !strings.Contains(unit, want) {
		t.Fatalf("systemd unit should forward the telemetry opt-out %q, got:\n%s", want, unit)
	}
}

// TestServiceForwardedEnvCollectsTelemetryKeys guards that every telemetry key
// set at install time is collected, after any proxy entries, in declaration
// order.
func TestServiceForwardedEnvCollectsTelemetryKeys(t *testing.T) {
	for _, key := range forwardedEnvKeys() {
		t.Setenv(key, "")
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7897")
	t.Setenv("NO_MISTAKES_TELEMETRY", "off")
	t.Setenv("NO_MISTAKES_UMAMI_HOST", "https://umami.example")
	t.Setenv("NO_MISTAKES_UMAMI_WEBSITE_ID", "site-123")

	got := serviceForwardedEnv()
	want := [][2]string{
		{"HTTPS_PROXY", "http://127.0.0.1:7897"},
		{"NO_MISTAKES_TELEMETRY", "off"},
		{"NO_MISTAKES_UMAMI_HOST", "https://umami.example"},
		{"NO_MISTAKES_UMAMI_WEBSITE_ID", "site-123"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("serviceForwardedEnv() = %v, want %v (proxy entries first, telemetry in declaration order)", got, want)
	}
}

// TestWriteServiceFileKeepsConventionalModeForTelemetryOnlyEnv pins the mode
// decision to proxy entries specifically: only proxy URLs can embed
// credentials, so a definition forwarding nothing but telemetry keys holds no
// secret and must keep the conventional world-readable 0644 rather than
// tightening to 0600.
func TestWriteServiceFileKeepsConventionalModeForTelemetryOnlyEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes (0644/0600) are not enforced on Windows; the forwarded-env service file is only generated on macOS/Linux")
	}
	for _, key := range forwardedEnvKeys() {
		t.Setenv(key, "")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "unit")
	render := func(forwardedEnv [][2]string) string { return "content" }

	// Telemetry only: no credential can be present, keep 0644.
	t.Setenv("NO_MISTAKES_TELEMETRY", "0")
	if err := writeServiceFile(path, nil, render); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("with telemetry only, mode = %o, want 0644", got)
	}

	// A proxy alongside the telemetry keys still tightens to owner-only 0600.
	t.Setenv("HTTPS_PROXY", "http://user:pass@127.0.0.1:7897")
	if err := writeServiceFile(path, nil, render); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("with proxy and telemetry, mode = %o, want 0600", got)
	}
}
