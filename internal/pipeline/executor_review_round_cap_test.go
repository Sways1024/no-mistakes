package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// respondEventually retries a review-gate response until the executor accepts
// it, so tests that answer the same gate status twice in a row do not race the
// park becoming observable.
func respondEventually(t *testing.T, exec *Executor, action types.ApprovalAction, findingIDs []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := exec.Respond(types.StepReview, action, findingIDs)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("respond %s was never accepted: %v", action, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A fix response under the cap proceeds; the fix response at the cap is
// rejected with a clear terminal-decision error, the run stays parked at the
// same gate, and the gate still accepts approve.
func TestExecutor_ReviewFixRoundCapRejectsFixAndKeepsGateParked(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	cfg := &config.Config{Review: config.Review{MaxRounds: 2}}
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"ask-user"}],"summary":"1 issue"}`
	step := newApprovalStep(types.StepReview, findings)

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// Round 1 parks; one more round stays under the cap, so fix is accepted.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatalf("fix under the cap was rejected: %v", err)
	}

	// Round 2 parks at the cap: a further fix must be rejected with the
	// terminal choices spelled out.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"})
	if err == nil {
		t.Fatal("fix at the cap was accepted, want rejection")
	}
	if !strings.Contains(err.Error(), "review fix-round cap (2) reached") {
		t.Fatalf("cap rejection = %q, want it to name the cap", err)
	}
	if !strings.Contains(err.Error(), "approve, skip, or abort") {
		t.Fatalf("cap rejection = %q, want it to state the terminal choices", err)
	}

	// The rejection must not corrupt state: the run is still parked awaiting
	// a decision, and the same gate still accepts a terminal action.
	parked, dbErr := database.GetRun(run.ID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if parked.Status != types.RunRunning || parked.AwaitingAgentSince == nil {
		t.Fatalf("run after cap rejection = status %s awaiting %v, want running and parked", parked.Status, parked.AwaitingAgentSince)
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve after cap rejection: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	rounds, err := database.GetRoundsByStep(firstStepID(t, database, run.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 {
		t.Fatalf("review rounds = %d, want exactly the cap of 2", len(rounds))
	}
}

// review.max_rounds 0 preserves the old unbounded behavior: every fix response
// is accepted no matter how many rounds are already recorded.
func TestExecutor_ReviewFixRoundCapZeroMeansUnlimited(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	cfg := &config.Config{Review: config.Review{MaxRounds: 0}}
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"ask-user"}],"summary":"1 issue"}`
	step := newApprovalStep(types.StepReview, findings)

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	for i := 0; i < 4; i++ {
		respondEventually(t, exec, types.ActionFix, []string{"review-1"})
	}
	respondEventually(t, exec, types.ActionApprove, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	rounds, err := database.GetRoundsByStep(firstStepID(t, database, run.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 5 {
		t.Fatalf("review rounds = %d, want 5 (initial + 4 uncapped fixes)", len(rounds))
	}
}

// A recovered gate counts the rounds already recorded in the DB, so the cap
// holds across daemon restarts instead of resetting with the in-memory counter.
func TestExecutor_ResumeEnforcesReviewFixRoundCapFromRecordedRounds(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"still broken","action":"ask-user"}],"summary":"1 issue"}`
	const reviewedHead = "4444444444444444444444444444444444444444"
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, reviewedHead, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 2, "auto_fix", &findings, nil, reviewedHead, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusFixReview, 20); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Review: config.Review{MaxRounds: 2}}
	exec := NewExecutor(database, p, cfg, nil, []Step{newApprovalStep(types.StepReview, findings)}, nil)
	done := make(chan error, 1)
	go func() {
		done <- exec.Resume(context.Background(), run, repo, t.TempDir())
	}()

	// Wait for the recovered gate to park, then require the fix rejection: the
	// two recorded rounds already spend the whole cap.
	deadline := time.Now().Add(5 * time.Second)
	var fixErr error
	for {
		fixErr = exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"})
		if fixErr == nil || !strings.Contains(fixErr.Error(), "no step awaiting approval") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered gate never parked: %v", fixErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fixErr == nil {
		t.Fatal("fix on the recovered gate was accepted, want the recorded rounds to spend the cap")
	}
	if !strings.Contains(fixErr.Error(), "review fix-round cap (2) reached") {
		t.Fatalf("cap rejection = %q, want it to name the cap", fixErr)
	}

	// The recovered gate still accepts a terminal decision.
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve after cap rejection: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered executor timed out")
	}

	resumed, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != types.RunCompleted || resumed.AwaitingAgentSince != nil {
		t.Fatalf("recovered run = status %s awaiting %v, want completed and unparked", resumed.Status, resumed.AwaitingAgentSince)
	}
}

// A recovered gate whose recorded rounds are under the cap still accepts fix,
// and the continued loop counts the recorded rounds toward the cap.
func TestExecutor_ResumeAllowsFixUnderCapThenEnforcesIt(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"still broken","action":"ask-user"}],"summary":"1 issue"}`
	const reviewedHead = "5555555555555555555555555555555555555555"
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, reviewedHead, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Review: config.Review{MaxRounds: 2}}
	exec := NewExecutor(database, p, cfg, nil, []Step{newApprovalStep(types.StepReview, findings)}, nil)
	done := make(chan error, 1)
	go func() {
		done <- exec.Resume(context.Background(), run, repo, t.TempDir())
	}()

	// One recorded round leaves one under the cap, so fix is accepted...
	respondEventually(t, exec, types.ActionFix, []string{"review-1"})

	// ...and the rereview parks as round 2, where the cap now rejects fix.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	err = exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"})
	if err == nil || !strings.Contains(err.Error(), "review fix-round cap (2) reached") {
		t.Fatalf("fix at the recovered cap = %v, want the cap rejection", err)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve after cap rejection: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered executor timed out")
	}

	rounds, err := database.GetRoundsByStep(stepResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 {
		t.Fatalf("review rounds = %d, want exactly the cap of 2", len(rounds))
	}
}
