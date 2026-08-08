package daemon

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestSystemdUnitForwardedEnvRoundTripsThroughRenderer guards the parser that
// lets drift detection inherit already-baked forwarded entries: parsing a
// rendered unit back and re-rendering with the result must reproduce the same
// unit byte-for-byte, including a percent-encoded credential whose `%` the
// renderer doubled (`%` -> `%%`) and the parser must undo, and a telemetry
// opt-out entry. The fixed HOME/PATH Environment= lines must not be mistaken
// for forwarded entries.
func TestSystemdUnitForwardedEnvRoundTripsThroughRenderer(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	in := [][2]string{
		{"HTTPS_PROXY", "http://user:p%40ss@127.0.0.1:7897"},
		{"NO_PROXY", "localhost,127.0.0.1"},
		{"http_proxy", "http://lower:1/"},
		{"NO_MISTAKES_TELEMETRY", "0"},
	}

	unit := renderSystemdUnitWithForwardedEnv("/usr/local/bin/no-mistakes", p, "/home/u", in)

	got := systemdUnitForwardedEnv([]byte(unit))
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("systemdUnitForwardedEnv() = %v, want %v (HOME/PATH must be skipped, %% undone)", got, in)
	}
	if reRendered := renderSystemdUnitWithForwardedEnv("/usr/local/bin/no-mistakes", p, "/home/u", got); reRendered != unit {
		t.Fatalf("re-rendering parsed forwarded env did not reproduce the unit:\nwant:\n%s\ngot:\n%s", unit, reRendered)
	}
}

// TestLaunchAgentForwardedEnvRoundTripsThroughRenderer is the launchd
// counterpart: parsing the EnvironmentVariables dict back and re-rendering must
// reproduce the same plist, and HOME/PATH must be skipped.
func TestLaunchAgentForwardedEnvRoundTripsThroughRenderer(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	in := [][2]string{
		{"HTTPS_PROXY", "http://user:p&ss@127.0.0.1:7897"},
		{"NO_PROXY", "localhost,127.0.0.1"},
		{"http_proxy", "http://lower:1/"},
		{"NO_MISTAKES_TELEMETRY", "0"},
	}

	plist := renderLaunchAgentWithForwardedEnv("/usr/local/bin/no-mistakes", p, "/home/u", in)

	got := launchAgentForwardedEnv([]byte(plist))
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("launchAgentForwardedEnv() = %v, want %v (HOME/PATH must be skipped)", got, in)
	}
	if reRendered := renderLaunchAgentWithForwardedEnv("/usr/local/bin/no-mistakes", p, "/home/u", got); reRendered != plist {
		t.Fatalf("re-rendering parsed forwarded env did not reproduce the plist:\nwant:\n%s\ngot:\n%s", plist, reRendered)
	}
}

// TestServiceForwardedEnvParsersReturnNilForEnvLessDefinition confirms the
// parsers return nothing for a definition rendered without any forwarded
// entries, so inheritance is a no-op when there was never a baked-in value.
func TestServiceForwardedEnvParsersReturnNilForEnvLessDefinition(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	unit := renderSystemdUnitWithForwardedEnv("/usr/local/bin/no-mistakes", p, "/home/u", nil)
	if got := systemdUnitForwardedEnv([]byte(unit)); len(got) != 0 {
		t.Fatalf("systemdUnitForwardedEnv(env-less unit) = %v, want empty", got)
	}
	plist := renderLaunchAgentWithForwardedEnv("/usr/local/bin/no-mistakes", p, "/home/u", nil)
	if got := launchAgentForwardedEnv([]byte(plist)); len(got) != 0 {
		t.Fatalf("launchAgentForwardedEnv(env-less plist) = %v, want empty", got)
	}
	if got := systemdUnitForwardedEnv(nil); len(got) != 0 {
		t.Fatalf("systemdUnitForwardedEnv(nil) = %v, want empty", got)
	}
	if got := launchAgentForwardedEnv([]byte("not a plist")); len(got) != 0 {
		t.Fatalf("launchAgentForwardedEnv(garbage) = %v, want empty", got)
	}
}
