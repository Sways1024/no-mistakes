package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// checksAPIRefusal is the error GitHub returns when a fine-grained personal
// access token reads check runs. Fine-grained tokens cannot call the Checks API
// at all (a documented limitation with no permission that grants it), so
// `gh pr checks` fails on every poll even when every check is green.
const checksAPIRefusal = "Resource not accessible by personal access token"

// rollupCheckName names the synthetic check that stands in for whatever part of
// the PR's aggregate status the Actions API cannot show (third-party checks
// such as codecov). It carries no link, so it is never treated as re-runnable.
const rollupCheckName = "other checks (aggregate status; not readable with this token)"

func isChecksAPIRefusal(out []byte) bool {
	return strings.Contains(string(out), checksAPIRefusal)
}

// restrictedTokenChecks rebuilds the PR's check list from the two surfaces a
// fine-grained token can still read: the Actions API (per-job names, buckets,
// completion times and job links, so the cancelled-job re-run path keeps
// working) and the GraphQL aggregate statusCheckRollup.state of the PR's head
// commit. The aggregate is the overall gate: when it is not SUCCESS and no
// visible Actions job explains why, a synthetic failing or pending check stands
// in for the unseen part so the step can never report green early.
func (h *Host) restrictedTokenChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	hostname, owner, name, err := h.repoCoordinates(pr)
	if err != nil {
		return nil, err
	}
	number, err := prNumber(pr)
	if err != nil {
		return nil, err
	}

	headSHA, rollupState, err := h.headRollup(ctx, hostname, owner, name, number)
	if err != nil {
		return nil, err
	}
	checks, err := h.actionsChecks(ctx, hostname, owner, name, headSHA)
	if err != nil {
		return nil, err
	}
	if synthetic, ok := unseenRollupCheck(rollupState, checks); ok {
		checks = append(checks, synthetic)
	}
	if len(checks) == 0 {
		return nil, nil
	}
	return checks, nil
}

// repoCoordinates resolves the API hostname (empty for github.com) and the
// owner/name of the repository the PR lives in.
func (h *Host) repoCoordinates(pr *scm.PR) (hostname, owner, name string, err error) {
	slug := h.repo
	if slug == "" && pr != nil {
		slug = RepoSlug(pr.URL)
	}
	parts := strings.Split(strings.Trim(slug, "/"), "/")
	switch len(parts) {
	case 2:
		owner, name = parts[0], parts[1]
	case 3:
		hostname, owner, name = parts[0], parts[1], parts[2]
	}
	if owner == "" || name == "" {
		return "", "", "", errors.New("repository owner/name unknown")
	}
	return hostname, owner, name, nil
}

func prNumber(pr *scm.PR) (string, error) {
	if pr != nil {
		if n := strings.TrimSpace(pr.Number); n != "" {
			return n, nil
		}
		return scm.ExtractPRNumber(strings.TrimSpace(pr.URL))
	}
	return "", errors.New("no PR number known")
}

func (h *Host) ghAPI(ctx context.Context, hostname string, args ...string) ([]byte, error) {
	full := []string{"api"}
	if hostname != "" {
		full = append(full, "--hostname", hostname)
	}
	full = append(full, args...)
	out, err := h.cmd(ctx, "gh", full...).Output()
	if err != nil {
		return nil, fmt.Errorf("gh api %s: %w", args[0], err)
	}
	return out, nil
}

const headRollupQuery = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){commits(last:1){nodes{commit{oid statusCheckRollup{state}}}}}}}`

// headRollup reads the PR's head commit and its aggregate check state in one
// GraphQL query. The state is empty when the commit has no checks at all.
func (h *Host) headRollup(ctx context.Context, hostname, owner, name, number string) (string, string, error) {
	out, err := h.ghAPI(ctx, hostname, "graphql",
		"-f", "query="+headRollupQuery,
		"-f", "owner="+owner,
		"-f", "name="+name,
		"-F", "number="+number,
	)
	if err != nil {
		return "", "", err
	}
	var payload struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					Commits struct {
						Nodes []struct {
							Commit struct {
								OID               string `json:"oid"`
								StatusCheckRollup *struct {
									State string `json:"state"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", "", fmt.Errorf("parse PR head status rollup: %w", err)
	}
	nodes := payload.Data.Repository.PullRequest.Commits.Nodes
	if len(nodes) == 0 || strings.TrimSpace(nodes[0].Commit.OID) == "" {
		return "", "", errors.New("PR head commit not found")
	}
	commit := nodes[0].Commit
	state := ""
	if commit.StatusCheckRollup != nil {
		state = strings.ToUpper(strings.TrimSpace(commit.StatusCheckRollup.State))
	}
	return strings.TrimSpace(commit.OID), state, nil
}

type actionsRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	UpdatedAt  string `json:"updated_at"`
}

type actionsJob struct {
	ID          int64  `json:"id"`
	RunID       int64  `json:"run_id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	HTMLURL     string `json:"html_url"`
	CompletedAt string `json:"completed_at"`
}

// actionsChecks lists the Actions jobs of every workflow run for headSHA as
// checks. A run that has no jobs yet is reported as one check for the run.
func (h *Host) actionsChecks(ctx context.Context, hostname, owner, name, headSHA string) ([]scm.Check, error) {
	base := "repos/" + owner + "/" + name + "/actions/runs"
	out, err := h.ghAPI(ctx, hostname, base+"?head_sha="+headSHA+"&per_page=100")
	if err != nil {
		return nil, err
	}
	var runs struct {
		WorkflowRuns []actionsRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(out, &runs); err != nil {
		return nil, fmt.Errorf("parse Actions runs: %w", err)
	}
	var checks []scm.Check
	for _, run := range runs.WorkflowRuns {
		out, err := h.ghAPI(ctx, hostname, fmt.Sprintf("%s/%d/jobs?per_page=100", base, run.ID))
		if err != nil {
			return nil, err
		}
		var jobs struct {
			Jobs []actionsJob `json:"jobs"`
		}
		if err := json.Unmarshal(out, &jobs); err != nil {
			return nil, fmt.Errorf("parse Actions jobs: %w", err)
		}
		if len(jobs.Jobs) == 0 {
			checks = append(checks, actionsCheck(run.Name, run.Status, run.Conclusion, run.HTMLURL, run.UpdatedAt))
			continue
		}
		for _, job := range jobs.Jobs {
			checks = append(checks, actionsCheck(job.Name, job.Status, job.Conclusion, jobLink(hostname, owner, name, job), job.CompletedAt))
		}
	}
	return checks, nil
}

// jobLink returns a details link that names the job in the
// ".../actions/runs/<run-id>/job/<job-id>" shape RerunCheck needs. The API's
// html_url is used when it already has that shape; otherwise (GitHub has served
// "/runs/<job-id>" forms) the link is built from the job's run and job ids.
func jobLink(hostname, owner, name string, job actionsJob) string {
	if _, jobID, ok := actionsRerunTarget(job.HTMLURL); ok && jobID != "" {
		return strings.TrimSpace(job.HTMLURL)
	}
	if job.ID == 0 || job.RunID == 0 {
		return strings.TrimSpace(job.HTMLURL)
	}
	if hostname == "" {
		hostname = "github.com"
	}
	return fmt.Sprintf("https://%s/%s/%s/actions/runs/%d/job/%d", hostname, owner, name, job.RunID, job.ID)
}

// actionsCheck maps an Actions status/conclusion pair onto the same State and
// Bucket vocabulary `gh pr checks` reports: the conclusion once completed,
// the in-flight status (QUEUED, IN_PROGRESS, ...) before that.
func actionsCheck(name, status, conclusion, link, completedAt string) scm.Check {
	state := strings.ToUpper(strings.TrimSpace(status))
	if state == "COMPLETED" {
		state = strings.ToUpper(strings.TrimSpace(conclusion))
	}
	check := scm.Check{
		Name:   strings.TrimSpace(name),
		Bucket: normalizeCheckBucket("", state),
		State:  state,
		Link:   strings.TrimSpace(link),
	}
	if check.Bucket != scm.CheckBucketPending && completedAt != "" {
		if parsed, err := time.Parse(time.RFC3339, completedAt); err == nil {
			check.CompletedAt = parsed
		}
	}
	return check
}

// unseenRollupCheck returns the synthetic check that represents the part of a
// non-SUCCESS aggregate that no visible check accounts for. A FAILURE/ERROR
// aggregate is accounted for by a visible failing or cancelled check; a
// PENDING/EXPECTED one by a visible pending check. Any other non-empty state is
// unknown and fails safe to pending.
func unseenRollupCheck(rollupState string, visible []scm.Check) (scm.Check, bool) {
	var bucket scm.CheckBucket
	switch rollupState {
	case "", "SUCCESS":
		return scm.Check{}, false
	case "FAILURE", "ERROR":
		bucket = scm.CheckBucketFail
		for _, c := range visible {
			if c.Bucket == scm.CheckBucketFail || c.Bucket == scm.CheckBucketCancel {
				return scm.Check{}, false
			}
		}
	default:
		bucket = scm.CheckBucketPending
		for _, c := range visible {
			if c.Bucket == scm.CheckBucketPending {
				return scm.Check{}, false
			}
		}
	}
	return scm.Check{Name: rollupCheckName, Bucket: bucket, State: rollupState}, true
}
