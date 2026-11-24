# Local patches (Sways1024 fork)

This branch (`patched`) is upstream `v1.48.0` (commit `2ac3769`) plus three bug fixes,
each on its own branch for upstream submission:

| Branch | Upstream issue | Fix |
| --- | --- | --- |
| `fix/655-preflight-schema-mismatch` | [#655](https://github.com/kunchenguid/no-mistakes/issues/655) | Daemon start no longer fails on a pre-v1.42.0 database: the read-only authorization preflight degrades gracefully on a schema mismatch instead of starving the migration it gates. |
| `fix/683-review-round-cap` | [#683](https://github.com/kunchenguid/no-mistakes/issues/683) | Server-side cap on total review fix rounds (`review.max_rounds`, default 10, 0 = unlimited). At the cap, a `fix` response is rejected with a clear message and the run stays parked until you approve, skip, or abort — ending the unbounded rereview loop. |
| `fix/610-daemon-telemetry-optout` | [#610](https://github.com/kunchenguid/no-mistakes/issues/610) | `NO_MISTAKES_TELEMETRY=0` (and the Umami override vars) are now baked into the managed launchd/systemd service definitions and survive `no-mistakes update` reinstalls. Set the variable before `no-mistakes init`. |
| `fix/674-opencode-hang-watchdog` | [#674](https://github.com/kunchenguid/no-mistakes/issues/674) | The opencode adapter can no longer hang forever: message-POST failures fail fast, a 10-minute event-silence watchdog aborts wedged streams with a diagnostic, session creation is bounded at 30s, and the managed server PID now surfaces as `agent_pid`. |
| `fix/666-ci-error-classification` | [#666](https://github.com/kunchenguid/no-mistakes/issues/666) | Persistent CI-poll failures (10 identical consecutive errors over ≥30 minutes) park the run for a human decision instead of silently retrying for up to a week. (The originally reported required-check bug is already absent in v1.48.0.) |
| `fix/642-recovery-amend-escape` | [#642](https://github.com/kunchenguid/no-mistakes/issues/642) | Amended-head runs are no longer permanently stranded: recovery adopts the gate head when it provably contains the preserved work, refusals now name the escape (`no-mistakes rerun` / `--keep-local`), `--keep-local` anchors the preserved commit by SHA, fix-agent prompts forbid history rewrites, and the CI fix path gained the missing head-continuity guard. |
| `fm/nm-ci-restricted-token` | fork addition, no upstream issue | CI monitoring works with a GitHub fine-grained personal access token, which cannot read the Checks API: when `gh pr checks` is refused with "Resource not accessible by personal access token", checks come from the Actions jobs for the PR head commit, and the GraphQL aggregate `statusCheckRollup.state` adds a synthetic pending or failing check for anything the token cannot see (such as codecov). Other errors and full-access tokens are unchanged. |

## Setup on a new Mac

```sh
# prerequisites
brew install go gh
gh auth login   # same GitHub account

# build
git clone https://github.com/Sways1024/no-mistakes.git
cd no-mistakes
git checkout patched
make build      # produces bin/no-mistakes

# install the binary somewhere on PATH
cp bin/no-mistakes /opt/homebrew/bin/

# privacy: opt out of telemetry BEFORE init so it is baked into the daemon service
export NO_MISTAKES_TELEMETRY=0
no-mistakes init   # run inside the repo you want to gate
```

Verify with `make test` (full suite; green as of this branch's creation).

## Keeping up with upstream

Upstream moves fast (near-daily releases). To refresh:

```sh
git fetch origin            # origin = kunchenguid/no-mistakes
git checkout patched
git rebase origin/main      # or re-merge the three fix branches onto a new patched branch
make test
```

Drop any fix branch that upstream has since fixed itself.
