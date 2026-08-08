# Local patches (Sways1024 fork)

This branch (`patched`) is upstream `v1.48.0` (commit `2ac3769`) plus three bug fixes,
each on its own branch for upstream submission:

| Branch | Upstream issue | Fix |
| --- | --- | --- |
| `fix/655-preflight-schema-mismatch` | [#655](https://github.com/kunchenguid/no-mistakes/issues/655) | Daemon start no longer fails on a pre-v1.42.0 database: the read-only authorization preflight degrades gracefully on a schema mismatch instead of starving the migration it gates. |
| `fix/683-review-round-cap` | [#683](https://github.com/kunchenguid/no-mistakes/issues/683) | Server-side cap on total review fix rounds (`review.max_rounds`, default 10, 0 = unlimited). At the cap, a `fix` response is rejected with a clear message and the run stays parked until you approve, skip, or abort — ending the unbounded rereview loop. |
| `fix/610-daemon-telemetry-optout` | [#610](https://github.com/kunchenguid/no-mistakes/issues/610) | `NO_MISTAKES_TELEMETRY=0` (and the Umami override vars) are now baked into the managed launchd/systemd service definitions and survive `no-mistakes update` reinstalls. Set the variable before `no-mistakes init`. |

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
