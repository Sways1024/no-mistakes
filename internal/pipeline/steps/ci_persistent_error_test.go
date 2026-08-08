package steps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPollErrStreakRequiresBothPollAndWindowGates(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	pollErr := errors.New("gh pr checks: exit status 1")

	// Enough polls, not enough elapsed time: the fast 30s cadence reaches the
	// poll gate within five minutes, which must not be escalation on its own.
	streak := pollErrStreak{}
	for poll := 0; poll < 3*persistentPollErrPolls; poll++ {
		at := started.Add(time.Duration(poll) * 30 * time.Second)
		if streak.observe(at, pollErr) {
			t.Fatalf("escalated after %d polls spanning %v, before the window", poll+1, at.Sub(started))
		}
	}

	// Enough elapsed time, not enough polls.
	streak = pollErrStreak{}
	for poll := 0; poll < persistentPollErrPolls-1; poll++ {
		if streak.observe(started.Add(time.Duration(poll)*time.Hour), pollErr) {
			t.Fatalf("escalated after %d polls, before the poll gate", poll+1)
		}
	}

	// Both gates met on the same observation.
	streak = pollErrStreak{}
	for poll := 0; poll < persistentPollErrPolls-1; poll++ {
		if streak.observe(started.Add(time.Duration(poll)*5*time.Minute), pollErr) {
			t.Fatalf("escalated early after %d polls", poll+1)
		}
	}
	if !streak.observe(started.Add(time.Duration(persistentPollErrPolls-1)*5*time.Minute), pollErr) {
		t.Fatal("did not escalate once both gates were met")
	}
}

// Two errors alternating never form a streak. Identity is the exact error
// string on purpose: the deterministic failures escalation exists for (expired
// auth, an incompatible gh, a renamed repo) repeat verbatim on every poll,
// while a message that keeps changing is a condition still changing. The
// strictness fails toward the pre-existing keep-polling behavior, which the
// idle timeout still bounds, so it can only delay a park - never cause one.
func TestPollErrStreakAlternatingErrorsNeverEscalate(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	errA := errors.New("gh pr checks: exit status 1")
	errB := errors.New("gh pr checks: context deadline exceeded")

	streak := pollErrStreak{}
	for poll := 0; poll < 4*persistentPollErrPolls; poll++ {
		pollErr := errA
		if poll%2 == 1 {
			pollErr = errB
		}
		if streak.observe(started.Add(time.Duration(poll)*5*time.Minute), pollErr) {
			t.Fatalf("alternating errors escalated after %d polls", poll+1)
		}
	}
}

func TestPollErrStreakResetRestartsBothGates(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	pollErr := errors.New("gh pr checks: exit status 1")

	streak := pollErrStreak{}
	for poll := 0; poll < persistentPollErrPolls-1; poll++ {
		streak.observe(started.Add(time.Duration(poll)*5*time.Minute), pollErr)
	}
	streak.reset()

	// The same failure count again, hours into the run: without the reset the
	// accumulated count and the old streak start would escalate immediately.
	resumed := started.Add(2 * time.Hour)
	for poll := 0; poll < persistentPollErrPolls-1; poll++ {
		if streak.observe(resumed.Add(time.Duration(poll)*5*time.Minute), pollErr) {
			t.Fatalf("escalated %d polls after a reset", poll+1)
		}
	}
	if !streak.observe(resumed.Add(time.Duration(persistentPollErrPolls-1)*5*time.Minute), pollErr) {
		t.Fatal("did not escalate after the restarted streak met both gates")
	}
}

// A provider query failing identically poll after poll parks the run at an
// ask-user gate carrying the persistent error, instead of retrying silently
// until the idle timeout (#666). All three per-poll queries take the same
// treatment.
func TestCIStep_PersistentProviderErrorParksForADecision(t *testing.T) {
	t.Parallel()

	green := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	tests := []struct {
		name        string
		env         func(t *testing.T) []string
		wantWhat    string
		wantPollErr string
	}{
		{
			name: "checks",
			env: func(t *testing.T) []string {
				return fakeCIGHChecksError(t, "OPEN", "MERGEABLE", "auth token expired")
			},
			wantWhat:    "could not check CI",
			wantPollErr: "gh pr checks: exit status 1",
		},
		{
			name: "pr_state",
			env: func(t *testing.T) []string {
				return fakeCIGHStateError(t, "provider unavailable", green)
			},
			wantWhat:    "could not check PR state",
			wantPollErr: "gh pr view: exit status 1",
		},
		{
			name: "mergeable_state",
			env: func(t *testing.T) []string {
				return fakeCIGHMergeableError(t, "OPEN", green, "provider unavailable")
			},
			wantWhat:    "could not check mergeable state",
			wantPollErr: "gh pr view mergeable: exit status 1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)

			prURL := "https://github.com/test/repo/pull/42"
			ag := &mockAgent{name: "test"}
			sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = tc.env(t)
			sctx.Run.PRURL = &prURL
			sctx.Config.CITimeout = 24 * time.Hour

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sctx.Ctx = ctx

			started := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
			current := started
			polls := 0
			step := &CIStep{
				now:           func() time.Time { return current },
				baseBranchTip: func(context.Context) (string, bool) { return "", false },
				// Bounded: a regression that never escalates must fail the test
				// rather than keep monitoring until the CI timeout.
				waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
					polls++
					if polls >= 30 {
						cancel()
						return ctx.Err()
					}
					current = current.Add(5 * time.Minute)
					return nil
				},
			}
			outcome, err := step.Execute(sctx)
			if err != nil {
				t.Fatalf("expected an approval outcome, got error: %v", err)
			}
			if !outcome.NeedsApproval {
				t.Fatal("expected a persistent provider failure to park for a decision")
			}
			var findings Findings
			if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
				t.Fatalf("unmarshal findings: %v", err)
			}
			if len(findings.Items) != 1 {
				t.Fatalf("findings = %+v, want exactly one item", findings.Items)
			}
			if findings.Items[0].Action != types.ActionAskUser {
				t.Fatalf("finding action = %q, want %q", findings.Items[0].Action, types.ActionAskUser)
			}
			desc := findings.Items[0].Description
			if !strings.Contains(desc, tc.wantWhat) || !strings.Contains(desc, tc.wantPollErr) {
				t.Fatalf("finding description = %q, want it to carry %q and %q", desc, tc.wantWhat, tc.wantPollErr)
			}
			if !strings.Contains(desc, "10 consecutive polls") {
				t.Fatalf("finding description = %q, want the streak length named", desc)
			}
		})
	}
}

// Failures that have not met both escalation gates keep the pre-existing
// warn-and-retry behavior: here the poll gate is exceeded but the window is
// not, so the monitor must still be polling when the test cancels it.
func TestCIStep_ProviderErrorsBelowTheGatesKeepPolling(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env := fakeCIGHChecksError(t, "OPEN", "MERGEABLE", "auth token expired")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 24 * time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	started := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	current := started
	polls := 0
	step := &CIStep{
		now:           func() time.Time { return current },
		baseBranchTip: func(context.Context) (string, bool) { return "", false },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			polls++
			// One-minute polls: twelve failures exceed the poll gate while
			// staying well under the half-hour window.
			if polls >= persistentPollErrPolls+2 {
				cancel()
				return ctx.Err()
			}
			current = current.Add(time.Minute)
			return nil
		},
	}
	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the monitor to still be polling below the window gate, got %v", err)
	}
	if polls != persistentPollErrPolls+2 {
		t.Fatalf("polls = %d, want %d before cancellation", polls, persistentPollErrPolls+2)
	}
}

// One successful poll resets the streak: escalation counts consecutive
// failures, so errors separated by a success must start over rather than
// accumulate across the run.
func TestCIStep_ChecksSuccessResetsThePersistentErrorStreak(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// Six failing polls, one success, then failures until escalation (the
	// sequence fake repeats its last entry).
	sequence := []string{
		"not-json", "not-json", "not-json", "not-json", "not-json", "not-json",
		`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
		"not-json",
	}
	env := fakeCIGHSequence(t, "OPEN", sequence)

	prURL := "https://github.com/test/repo/pull/42"
	ag := &mockAgent{name: "test"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 24 * time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	started := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	current := started
	polls := 0
	step := &CIStep{
		now:           func() time.Time { return current },
		baseBranchTip: func(context.Context) (string, bool) { return "", false },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			polls++
			if polls >= 30 {
				cancel()
				return ctx.Err()
			}
			current = current.Add(5 * time.Minute)
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected an approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected the post-success failure streak to eventually park")
	}
	// Six pre-success failures plus a streak that restarts at poll 8: both
	// gates are met again on poll 17, so 16 waits precede the park. Without the
	// reset the accumulated count would have parked the run on poll 11.
	if polls != 16 {
		t.Fatalf("polls before the park = %d, want 16 (streak restarted after the success)", polls)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 || !strings.Contains(findings.Items[0].Description, "10 consecutive polls") {
		t.Fatalf("findings = %+v, want the restarted 10-poll streak named", findings.Items)
	}
}
