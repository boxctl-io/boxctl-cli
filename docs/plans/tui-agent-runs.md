# Agent runs from the tui: repos, backlog items, agents

Status: design note. Nothing here is implemented. It builds on what is
already shipped: `:repos` in `boxctl tui` (tuirepos.go), `boxctl claude
--repo --issue` unattended runs (claudegoal.go), run status over vsock and
box labels (boxctl-vms `docs/plans/run-status.md`), and the `pr-visual`
harness.

## What it looks like

You pick a repository, see its waiting backlog items, start a Claude
agent on one or several of them, and watch and steer the agents from one
screen. Every agent works on its own box, the same way
`boxctl claude --repo owner/name --issue N` does today. An agent's run
counts as done only when its pull request's checks are green, as checked
by the runner script rather than by the model.

```
:repos                      Enter                 :items cdalar/boxctl-vms
 REPO               PUSHED   ───────────▶   #    TITLE                        LABELS   AGENT
 cdalar/boxctl-vms  2h       │              312  Backups: retry on 503       bug      -
 boxctl-io/boxctl   5h       │              309  Labels on boxes: filter     feature  running 14m
 cdalar/onctl       1d       │              301  Docs: install page          docs     pr #318 ✓
                             │       space marks rows, a starts an agent on each
                             ▼
:agents
 BOX                       REPO/ISSUE        STATE        PHASE      LAST   PR     CHECKS
 goal-boxctl-vms-309-a1f2  boxctl-vms#309    running      testing    20s    -      -
 goal-boxctl-vms-301-77c0  boxctl-vms#301    done         -          3m     #318   ✓ 6/6
 goal-onctl-288-0b3e       onctl#288         needs-human  fixing-ci  1m     #1120  ✗ lint
   l logs   m message   r rework   A attach   o open PR   x stop   d describe
```

In this note, a "backlog item" (PBI) is an open GitHub issue. The
tracker is GitHub issues: Linear is retired for this work. "Waiting"
means open, with no open pull request that closes it and no agent on it.

## Why

Today, steps that belong together are spread over commands and sessions:
`:repos` → `g` starts a free-text goal, `boxctl claude --issue N` takes
one issue at a time from a shell, `claude ls` and `claude logs` read
progress, and a manager Claude session does the review loop by hand
("show me the status of the agents"). There is no way to talk to a
running agent, and a run counts as `done` once a pull request exists,
even if CI goes red on it a minute later. The 2026-11 issue waves showed
the cost of that. Agents repeated each other's setup work (installing
Go), and pull requests needed a second pass for lint and test failures
that a local run would have caught.

## Shape

```
 boxctl tui ──(gh)──────────────────────────▶ GitHub: repos, issues, PRs, checks
     │
     ├─ start:   boxctl claude --repo R --issue N --detach ...  (child process, as `g` does now)
     ├─ watch:   GET /api/vms?label=boxctl.run=goal   (run_status, no ssh, works while paused)
     ├─ logs:    boxctl claude logs -f <box>          (ssh, claudelogfmt)
     └─ steer:   boxctl claude say <box> "..."        (ssh: append to the run's inbox)
                 boxctl claude rework <box> "..."     (ssh: resume the finished session)

 on the box (goal runner, claudegoal.go runnerScript):
   claude -p ... ──▶ local checks ──▶ PR ──▶ wait for CI ──▶ done
        ▲               │fail                     │fail
        └───── resume with the failure output ────┘   (at most N rounds, then needs-human)
```

The CLI stays the API. The tui is a front end over `boxctl claude`
subcommands, so a manager Claude session, a script or a later queue
worker (cdalar/boxctl-vms#295) can do anything the tui can. The server
needs no change: labels and `run_status` already carry what it needs,
and the controller stores `run_status` as the opaque line the box sent.

## 1. `:items` — a repository's waiting backlog

- From `:repos`: Enter on a repository opens `:items owner/name`.
  Opening Claude attached, which Enter does today, moves to `c`. `g` (free-text
  goal) stays. `:items owner/name` also works from the prompt.
- Source: `gh issue list --repo R --state open --limit 200 --json
  number,title,labels,assignees,updatedAt,closedByPullRequestsReferences`.
  Rows with an open closing pull request show it in AGENT, and the `0/1`
  filter keys switch between "waiting" (the default) and "all open".
- AGENT column: joined from the boxes listing by labels `boxctl.repo=R`
  and `boxctl.issue=N`. It shows the run's state, or the pull request and its
  checks once there is one.
- Refresh: when the view opens and on `R`. After that, every 60 s while
  the view is shown, never on the 5 s tick. Issues change more often
  than repos do, so they are not cached on disk.
- `/` filters as in every view. `L` filters by label, for example
  `agent-ready`, if a repository wants to mark what agents may take.
  The last label filter used is remembered per repository in
  `~/.boxctl/tui.json`.
- `d` shows the issue body and comments (`gh issue view --comments`) as a
  text page. `o` opens it in the browser.

## 2. Starting agents

- `a` on a row, or on every row marked with space, opens one dialog for
  the batch:

  ```
  Start agents on 3 items of cdalar/boxctl-vms
    model    sonnet        (tab: sonnet / opus / haiku)
    effort   medium
    harness  pr-verified
    note     ______________________   (added to each goal)
  [Enter] start   [Esc] cancel
  ```

  The defaults come from the harness's model file, `BOXCTL_CLAUDE_MODEL`
  and `claude_model`, the same resolution order as `--model`/`--effort`
  (claudemodel.go). The tui remembers the last choice per repository.
- Each item becomes one child process:
  `boxctl claude --repo R --issue N --model M --effort E --harness H
  [--goal "<note>"]`, the way `g` starts a goal run now. The flash names
  each box, and the rows switch to `starting` at once.
- **No duplicates:** before starting, the tui checks the boxes listing
  for a live or paused box with the same `boxctl.repo` and `boxctl.issue`.
  If one exists, it asks whether to open that run instead. The CLI does
  the same check, so a script can't start two agents on one item either.
- **A parallel limit:** `claude_max_runs` in the config (default 4)
  caps the live goal boxes. Items started past the limit wait in
  `queued` in the tui and start as running ones finish. The queue lives
  only in the tui process. If you quit the tui, the queued items
  don't start. That is deliberate: a queue that outlives the tui is
  #295's job.
- **Mark the issue (optional, per repository):** with
  `claude_issue_labels: true`, the runner adds `agent:running` to the
  issue and swaps it for `agent:pr` or `agent:needs-human` at the end.
  Then the GitHub web UI shows the same state. Off by default, because
  it writes to someone else's tracker.

## 3. `:agents` — watching runs

A view over `GET /api/vms?label=boxctl.run=goal`, the same call that
`claude ls` makes, refreshed on the 5 s tick. It makes no ssh calls, so
paused boxes show their final state too.

| Column | From |
|---|---|
| BOX | box name |
| REPO/ISSUE | labels `boxctl.repo`, `boxctl.issue` |
| STATE | `run_status.status` |
| PHASE | `run_status.phase` (new, below) |
| LAST | `run_status.updated`; turns yellow after 5 min, red after 15 |
| PR, CHECKS | `run_status.pr_url`, `run_status.checks` (new) |
| ROUND | `run_status.round`, for example `2/3` |
| MODEL | label `boxctl.model` (new) |

States (`run_status.status`): `running`, `verifying` (local checks or
waiting for CI), `done` (pull request open, checks green),
`needs-human` (rounds used up, a question for you, or blocked) and
`failed` (crashed, no pull request). `needs-human` and `failed` sort
first and show red. The header counts them: `agents 7 · 2 need you`.

Keys:

| Key | Action |
|---|---|
| `l` | logs, followed, as `boxctl claude logs -f` (formatted) |
| `A` | attach to the box's Claude session, read-only by default; `w` in the attach makes it writable |
| `m` | message the running agent (section 4) |
| `r` | rework a finished run with feedback (section 4) |
| `o` | open the pull request; `O` opens the issue |
| `e` | evidence: list `/root/evidence/README.md`, fetch the files with `boxctl cp` |
| `x` | stop the run (asks first); the box is kept until the reaper pauses it |
| `ctrl+d` | delete the box (asks first) |

### Richer status from the box

`status.json` gets four fields, sent through the same vsock path:

```json
{"status":"verifying","phase":"fixing-ci","round":2,"max_rounds":3,
 "checks":{"state":"failure","passed":5,"total":6,"failing":["lint"]},
 "note":"golangci-lint: unused param in cmd/foo.go",
 "branch":"boxctl/issue-309-a1f2","pr_url":"...","updated":"..."}
```

- `phase` is set by Claude through a small helper on the image,
  `boxctl-phase <phase> "<note>"`, which rewrites `status.json` under the
  existing lock and calls `boxctl-status`. The harness says when to call
  it: `reading`, `planning`, `implementing`, `testing`,
  `opening-pr`, `fixing-ci`, `waiting-for-guidance`.
- `checks`, `round` and the `verifying`/`needs-human` states are written
  by the runner script, never by the model.
- The runner also touches `updated` whenever the log grows, at most once
  a minute, so LAST shows a stuck agent even if it never calls
  `boxctl-phase`.

## 4. Steering: messages and rework

Two cases, with different mechanisms, because `claude -p` runs one
session to the end and reads no further input.

**While a run is going: `m` / `boxctl claude say <box> "..."`.**
The message is appended over ssh to `<goalDir>/inbox.jsonl` (with who
sent it and when). A `PostToolUse` hook, installed by the harness in
`managed-settings.json`, checks the inbox after each tool call. It
passes new lines to Claude as `additionalContext`, tagged as coming from
the run's manager, and moves them to `inbox.done.jsonl`. So a message
lands at the agent's next tool call, usually within seconds. The tui
shows `delivered` once it has moved. Examples: "use the existing
`retry` helper in internal/client, don't add a dependency", "#309 is
blocked on #305; stop and say so".

**After a run has ended: `r` / `boxctl claude rework <box> "..."`.**
This works for a `done` run you want changed after review, and for
`needs-human`. The box is resumed if it is paused. Then the runner runs
again with `claude -p --resume <session id> "<feedback>"` on the same
branch. The session id is recorded from the first run's stream-json
`init` line. The pull request is updated, not reopened, and `round`
counts on. `boxctl claude rework <box> --from-review` uses the pull
request's unresolved review comments as the feedback.

**Who the manager is.** Either you in the tui, or a Claude session
acting as manager (the 2026-11 wave setup), which uses `boxctl claude ls
--json`, `logs`, `say` and `rework` from its shell. Both go through the
same commands, so their messages land in the same inbox and log. When an
agent needs a decision, it calls `boxctl-phase waiting-for-guidance
"<question>"` and keeps working on whatever doesn't depend on the
answer. If it can't, it ends the run, and the runner marks it
`needs-human` with the question as `note`. The tui shows the question in
the row's detail and in the flash.

**What carries over between agents.** The waves showed agents repeating
the same discoveries. Each finished run writes
`/root/evidence/learnings.md` (setup it needed, traps in the repo). The
manager decides what goes into the repository's `CLAUDE.md` or the
image. Nothing is shared between running agents automatically.

## 5. Making the pull request pass: the `pr-verified` harness and runner

The model writes the code. Deciding whether the result is good enough
is up to the runner script, which doesn't take the model's word for it.

### Harness rules (`pr-verified` = `pr-visual` + these)

- Before writing code, find how the repository is checked. Read
  `.github/workflows/*`, `Makefile`/`Taskfile`, `package.json` scripts
  and `AGENTS.md`/`CLAUDE.md`. Write the commands to
  `/root/evidence/checks.txt`.
- Every behaviour change comes with a test: one that fails without the
  change and passes with it, where that is practical. If no test is
  practical, say why in the pull request's "Tests" section.
- Run the build, the whole test suite, the linters and the formatters
  locally, and fix what they report, before opening the pull request.
  Don't skip or delete a failing test to get green. Don't add `nolint`
  or `skip` without saying why in the pull request.
- Keep the change to the issue. Unrelated fixes go in the pull request
  body as suggestions.
- The pull request body has the sections Summary, Tests (what was added,
  and the local command output summary), Evidence and Not done.

### Runner (claudegoal.go `runnerScript`)

```
1. claude -p (round 1)
2. local checks: run the repository's checks (below); if they fail and
   rounds are left → claude -p --resume "these checks failed: <tail>" → 2
3. no pull request → failed
4. CI: gh pr checks <pr> --watch --fail-fast (timeout 30 min)
   - green          → done
   - red, rounds left → gh run view --log-failed | tail → resume → push → 4
   - red, no rounds → needs-human (note: the failing check)
   - no checks configured → done, with checks.state = "none"
```

- **Which checks the runner runs:** `.boxctl/checks` in the repository,
  one shell command per line, if it exists. Otherwise the runner runs
  what Claude wrote to `checks.txt`, after removing lines that aren't
  plain build/test/lint commands. A repository that wants the result to
  be dependable adds `.boxctl/checks`. That file is the contract between
  the repository and the runner.
- `max_rounds` defaults to 3, set by `--max-rounds` or by the harness.
  A round is a resume of the same session, so Claude keeps its context.
- The runner waits for CI with the box's GitHub token, which goal mode
  already requires. It never merges and never re-runs a workflow except
  through a new push.
- Each round's check output goes to `<goalDir>/round-<n>.txt`. `e` in
  the tui lists those files.

## Where the code goes

| Repo | Change |
|---|---|
| boxctl-io/boxctl-cli | `:items` and `:agents` views, the start dialog and queue, `claude say`, `claude rework`, `claude ls --json`, the duplicate check, `claude_max_runs`, label `boxctl.model`, runner script rounds/CI wait/session id, the `pr-verified` harness |
| cdalar/boxctl-vms (`claude-agent` image) | `boxctl-phase`, the inbox `PostToolUse` hook script, `asciinema`/`agg` already there |
| boxctl-io/boxctl (server) | none: `run_status` is stored as sent, labels exist |

## Milestones

1. `:items` view: list, waiting filter, AGENT join, `d`/`o`. Enter on
   `:repos` opens it.
2. Start from the tui: `a` with a batch dialog, the duplicate check
   (tui and CLI), `claude_max_runs` and the in-tui queue.
3. `:agents` view over the existing `run_status`, with `l`, `o`, `x`,
   `A`, `e`, plus `claude ls --json`.
4. The runner's local checks and CI wait with rounds, and the `verifying`
   and `needs-human` states. Then the `pr-verified` harness and
   `.boxctl/checks`.
5. `boxctl-phase` and the richer status fields. PHASE/LAST/CHECKS
   columns.
6. Steering: `claude rework` (resume) first, because it needs nothing
   new from Claude Code. Then `claude say` with the inbox hook, after
   the spike in Open questions.
7. Optional issue labels (`agent:*`) and `learnings.md`.

Milestones 1–3 are useful on their own. They put today's runs in one
screen. Milestone 4 is the one that changes the quality of the pull
requests.

## Open questions

- **Inbox hook spike:** confirm that a `PostToolUse` hook's
  `additionalContext` reaches the model in `claude -p` mode with
  `--dangerously-skip-permissions`, on the Claude Code version baked
  into the image. The fallback is the `Stop` hook: on stop, if the inbox
  has messages, block the stop and pass them as the reason. That
  delivers only when Claude would otherwise finish.
- Should `A` (attach) be allowed to type into a `claude -p` run at all,
  or only watch? Typing there doesn't reach Claude, so read-only may
  be the only honest option, with `m` for talking.
- `checks.txt` filtering: how strict? Running model-written commands
  as the runner is a trust question, even on a throwaway box. Should
  the runner refuse anything but `.boxctl/checks` instead?
- CI that needs secrets the fork/branch doesn't get: is that
  `needs-human` at once, or `done` with a warning?
- Does `claude_max_runs` count only this user's goal boxes, or every
  goal box on the account? (aimax capacity is shared.)
- Does #295 (the opencode queue loop) become the unattended dispatcher on
  top of these same commands, or is it closed?
