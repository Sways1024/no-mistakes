package github

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	restrictedChecksKey = "gh pr checks 455 --repo o/r --json name,state,bucket,completedAt,link"
	restrictedRunsKey   = "gh api repos/o/r/actions/runs?head_sha=682a15fd&per_page=100"
	restrictedJobsKey   = "gh api repos/o/r/actions/runs/11/jobs?per_page=100"
)

var restrictedRollupKey = "gh api graphql -f query=" + headRollupQuery + " -f owner=o -f name=r -F number=455"

// checksRefusal is the shape of the error gh prints when a fine-grained token
// reads check runs.
var checksRefusal = githubTestResponse{
	stderr: "GraphQL: Resource not accessible by personal access token (repository.pullRequest.statusCheckRollup.contexts.nodes.0)\n",
	code:   1,
}

func rollupResponse(state string) githubTestResponse {
	return githubTestResponse{stdout: `{"data":{"repository":{"pullRequest":{"commits":{"nodes":[{"commit":{"oid":"682a15fd","statusCheckRollup":{"state":"` + state + `"}}}]}}}}}`}
}

const ciRun = `{"workflow_runs":[{"id":11,"name":"CI Pipeline","status":"completed","conclusion":"success","html_url":"https://github.com/o/r/actions/runs/11","updated_at":"2026-11-24T10:00:00Z"}]}`

func jobsResponse(jobs ...string) githubTestResponse {
	return githubTestResponse{stdout: `{"jobs":[` + strings.Join(jobs, ",") + `]}`}
}

func job(id, name, status, conclusion string) string {
	return `{"id":` + id + `,"name":"` + name + `","status":"` + status + `","conclusion":"` + conclusion +
		`","html_url":"https://github.com/o/r/actions/runs/11/job/` + id + `","completed_at":"2026-11-24T10:00:00Z"}`
}

// recordingFactory wraps githubTestCmdFactory and records every command run.
func recordingFactory(responses map[string]githubTestResponse) (CmdFactory, func() []string) {
	inner := githubTestCmdFactory(responses)
	var mu sync.Mutex
	var seen []string
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
			mu.Lock()
			seen = append(seen, strings.TrimSpace(name+" "+strings.Join(args, " ")))
			mu.Unlock()
			return inner(ctx, name, args...)
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), seen...)
		}
}

func restrictedHost(responses map[string]githubTestResponse) (*Host, func() []string) {
	factory, seen := recordingFactory(responses)
	return New(factory, nil, "", "o/r"), seen
}

func bucketsByName(checks []scm.Check) map[string]scm.CheckBucket {
	out := make(map[string]scm.CheckBucket, len(checks))
	for _, c := range checks {
		out[c.Name] = c.Bucket
	}
	return out
}

func TestGetChecks_RestrictedTokenFallsBackToActionsAllGreen(t *testing.T) {
	t.Parallel()

	host, _ := restrictedHost(map[string]githubTestResponse{
		restrictedChecksKey: checksRefusal,
		restrictedRollupKey: rollupResponse("SUCCESS"),
		restrictedRunsKey:   {stdout: ciRun},
		restrictedJobsKey: jobsResponse(
			job("101", "build", "completed", "success"),
			job("102", "test", "completed", "success"),
			job("103", "deploy-preview", "completed", "skipped"),
		),
	})

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "455"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	got := bucketsByName(checks)
	want := map[string]scm.CheckBucket{"build": scm.CheckBucketPass, "test": scm.CheckBucketPass, "deploy-preview": scm.CheckBucketSkip}
	if len(got) != len(want) {
		t.Fatalf("checks = %+v, want exactly %v", checks, want)
	}
	for name, bucket := range want {
		if got[name] != bucket {
			t.Fatalf("check %q bucket = %q, want %q (checks %+v)", name, got[name], bucket, checks)
		}
	}
	build := checks[0]
	if build.State != "SUCCESS" || build.Link != "https://github.com/o/r/actions/runs/11/job/101" || build.CompletedAt.IsZero() {
		t.Fatalf("build check = %+v, want SUCCESS state, job link and completion time", build)
	}
}

func TestGetChecks_RestrictedTokenReportsFailingActionsJob(t *testing.T) {
	t.Parallel()

	host, _ := restrictedHost(map[string]githubTestResponse{
		restrictedChecksKey: checksRefusal,
		restrictedRollupKey: rollupResponse("FAILURE"),
		restrictedRunsKey:   {stdout: ciRun},
		restrictedJobsKey: jobsResponse(
			job("101", "build", "completed", "success"),
			job("102", "test", "completed", "failure"),
		),
	})

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "455"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	got := bucketsByName(checks)
	if got["test"] != scm.CheckBucketFail || got["build"] != scm.CheckBucketPass {
		t.Fatalf("checks = %+v, want failing test and passing build", checks)
	}
	if _, ok := got[rollupCheckName]; ok {
		t.Fatalf("checks = %+v, want no synthetic check when a visible job explains the failure", checks)
	}
}

func TestGetChecks_RestrictedTokenKeepsCancelledJobRerunnable(t *testing.T) {
	t.Parallel()

	host, _ := restrictedHost(map[string]githubTestResponse{
		restrictedChecksKey: checksRefusal,
		restrictedRollupKey: rollupResponse("FAILURE"),
		restrictedRunsKey:   {stdout: ciRun},
		restrictedJobsKey:   jobsResponse(job("102", "test", "completed", "cancelled")),
	})

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "455"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("checks = %+v, want only the cancelled job (no synthetic failure that would suppress its rerun)", checks)
	}
	c := checks[0]
	if c.Bucket != scm.CheckBucketCancel || c.State != "CANCELLED" {
		t.Fatalf("check = %+v, want cancel bucket with CANCELLED state", c)
	}
	if args, ok := rerunTargetArgs(c.Link); !ok || strings.Join(args, " ") != "--job 102" {
		t.Fatalf("rerunTargetArgs(%q) = %v, %v; want --job 102", c.Link, args, ok)
	}
}

func TestGetChecks_RestrictedTokenAggregateHoldsGreenActions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state  string
		bucket scm.CheckBucket
	}{
		{"PENDING", scm.CheckBucketPending},
		{"EXPECTED", scm.CheckBucketPending},
		{"FAILURE", scm.CheckBucketFail},
		{"ERROR", scm.CheckBucketFail},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			t.Parallel()
			host, _ := restrictedHost(map[string]githubTestResponse{
				restrictedChecksKey: checksRefusal,
				restrictedRollupKey: rollupResponse(tc.state),
				restrictedRunsKey:   {stdout: ciRun},
				restrictedJobsKey:   jobsResponse(job("101", "build", "completed", "success")),
			})

			checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "455"})
			if err != nil {
				t.Fatalf("GetChecks() error = %v", err)
			}
			got := bucketsByName(checks)
			if got["build"] != scm.CheckBucketPass {
				t.Fatalf("checks = %+v, want passing build", checks)
			}
			if got[rollupCheckName] != tc.bucket {
				t.Fatalf("checks = %+v, want synthetic %q check for unseen %s aggregate", checks, tc.bucket, tc.state)
			}
			for _, c := range checks {
				if c.Name == rollupCheckName && (c.Link != "" || c.State != tc.state) {
					t.Fatalf("synthetic check = %+v, want no link and state %s", c, tc.state)
				}
			}
		})
	}
}

func TestGetChecks_RestrictedTokenPendingJobReportsPending(t *testing.T) {
	t.Parallel()

	host, _ := restrictedHost(map[string]githubTestResponse{
		restrictedChecksKey: checksRefusal,
		restrictedRollupKey: rollupResponse("PENDING"),
		restrictedRunsKey:   {stdout: `{"workflow_runs":[{"id":11,"name":"CI Pipeline","status":"queued","conclusion":null}]}`},
		restrictedJobsKey:   jobsResponse(),
	})

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "455"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "CI Pipeline" || checks[0].Bucket != scm.CheckBucketPending || checks[0].State != "QUEUED" {
		t.Fatalf("checks = %+v, want a single pending run-level check", checks)
	}
}

func TestGetChecks_RestrictedTokenBuildsJobLinkFromIDs(t *testing.T) {
	t.Parallel()

	host, _ := restrictedHost(map[string]githubTestResponse{
		restrictedChecksKey: checksRefusal,
		restrictedRollupKey: rollupResponse("FAILURE"),
		restrictedRunsKey:   {stdout: ciRun},
		restrictedJobsKey:   {stdout: `{"jobs":[{"id":102,"run_id":11,"name":"test","status":"completed","conclusion":"cancelled","html_url":"https://github.com/o/r/runs/102"}]}`},
	})

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "455"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Link != "https://github.com/o/r/actions/runs/11/job/102" {
		t.Fatalf("checks = %+v, want job link built from run and job ids", checks)
	}
}

func TestGetChecks_RestrictedTokenFallbackErrorIsReported(t *testing.T) {
	t.Parallel()

	host, _ := restrictedHost(map[string]githubTestResponse{
		restrictedChecksKey: checksRefusal,
		restrictedRollupKey: rollupResponse("SUCCESS"),
		restrictedRunsKey:   {stderr: "HTTP 403: Resource not accessible by personal access token", code: 1},
	})

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "455"})
	if err == nil {
		t.Fatalf("GetChecks() = %+v, nil; want an error when the fallback fails", checks)
	}
	if !strings.Contains(err.Error(), "fallback failed") {
		t.Fatalf("GetChecks() error = %v, want it to name the failed fallback", err)
	}
}

func TestGetChecks_NonRefusalErrorKeepsExistingHandling(t *testing.T) {
	t.Parallel()

	host, seen := restrictedHost(map[string]githubTestResponse{
		restrictedChecksKey: {stderr: "HTTP 502: Bad Gateway", code: 1},
	})

	_, err := host.GetChecks(context.Background(), &scm.PR{Number: "455"})
	if err == nil || err.Error() != "gh pr checks: exit status 1" {
		t.Fatalf("GetChecks() error = %v, want the unchanged gh pr checks error", err)
	}
	if calls := seen(); len(calls) != 1 {
		t.Fatalf("commands = %v, want only gh pr checks (no fallback)", calls)
	}
}

func TestGetChecks_FullAccessTokenSkipsFallback(t *testing.T) {
	t.Parallel()

	host, seen := restrictedHost(map[string]githubTestResponse{
		restrictedChecksKey: {stdout: `[{"name":"build","state":"SUCCESS","bucket":"pass","link":"https://github.com/o/r/actions/runs/11/job/101"}]`},
	})

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "455"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "build" || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("checks = %+v, want the gh pr checks result", checks)
	}
	if calls := seen(); len(calls) != 1 {
		t.Fatalf("commands = %v, want only gh pr checks (no fallback)", calls)
	}
}

func TestGetChecks_RestrictedTokenUsesGHEHostname(t *testing.T) {
	t.Parallel()

	factory, _ := recordingFactory(map[string]githubTestResponse{
		"gh pr checks 455 --repo ghe.example.com/o/r --json name,state,bucket,completedAt,link":                         checksRefusal,
		"gh api --hostname ghe.example.com graphql -f query=" + headRollupQuery + " -f owner=o -f name=r -F number=455": rollupResponse("SUCCESS"),
		"gh api --hostname ghe.example.com repos/o/r/actions/runs?head_sha=682a15fd&per_page=100":                       {stdout: `{"workflow_runs":[]}`},
	})
	host := New(factory, nil, "ghe.example.com", "ghe.example.com/o/r")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "455"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("checks = %+v, want none", checks)
	}
}
