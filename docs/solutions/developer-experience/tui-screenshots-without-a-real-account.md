---
title: Running and screenshotting the tui without touching a real account
date: 2026-11-21
category: developer-experience
module: cmd/tui
problem_type: developer_experience
component: development_workflow
severity: low
applies_when:
  - You need to see a tui view (:backlog, :runs, :boxes) with realistic data, e.g. for a PR or a demo
  - The real ~/.boxctl holds a token, and a backlog the tui would act on
tags: [tui, screenshot, tmux, backlog, demo, fake-server]
---

# Running and screenshotting the tui without touching a real account

## Context

`boxctl tui` is not a passive viewer. With a real `~/.boxctl` it lists
the account's boxes and, on any view, runs backlog passes every
`backlogPassEvery`: a queued item with a free slot launches a real
`boxctl claude` run, and a pass can delete boxes (a killed run's box). So
"just run it and take a screenshot" against the real account can start
or delete work. An empty real backlog also shows nothing worth looking
at. The tests (`cmd/tui_test.go`, `cmd/tuiruns_test.go`) drive the model,
not a terminal, so they give no picture either.

## Guidance

Run the real binary against a throwaway home and a fake API:

1. **A throwaway `HOME`** with `.boxctl/config.json` pointing
   `api_url` at a local port and any `token`, and a hand-written
   `$HOME/.boxctl/backlog.json` with the items and runs you want to show
   (fields are `internal/backlog`'s JSON tags: `next`, `items`, `limits`,
   each item's `runs` with `kind`, `box`, `started`, `launched`, `ended`,
   `outcome`).
2. **Keep the tui from running passes**: set `"loop": "demo-elsewhere"`
   and `"loop_seen"` to now in that `backlog.json`. `Backlog.Claim`
   refuses while another owner was seen within `LoopStale` (2 minutes),
   so the tui only displays (its header says "run by demo-elsewhere").
   Take the screenshots within those 2 minutes, or refresh `loop_seen`.
3. **A fake `/api/vms`**: a few lines of `python3 http.server` that
   answer `GET /api/vms*` with a JSON list of boxes (`name`, `state`,
   `run_status` such as `{"status":"running"}`) and `[]` for anything
   else. Name the boxes after the runs (`q12-ci-3`) so `:runs` shows them
   as live.
4. **A `gh` stub that exits 1** first on `PATH`: the tui and passes call
   `gh` (PR states, `:repos`), and the real one would query GitHub with
   your login.
5. **Run it in tmux** at a fixed size, drive it with `send-keys`, and
   capture with colours:

   ```bash
   tmux new-session -d -s demo -x 130 -y 32 \
     "env HOME=$S/home PATH=$S/bin:/usr/bin:/bin TERM=xterm-256color COLORTERM=truecolor $S/bin/boxctl tui"
   tmux send-keys -t demo ':'; tmux send-keys -t demo 'backlog' Enter
   tmux capture-pane -t demo -e -p > backlog.ans
   ```

6. **Turn the capture into a PNG**: convert the ANSI SGR codes (16, 256
   and 24-bit colours, bold, reverse) to `<span style>` in a `<pre>`, then
   `Google Chrome --headless=new --screenshot=out.png
   --window-size=1150,580 --force-device-scale-factor=2 file://...html`.
   At 14px Menlo a 130-column pane needs about 1150px of width; 1090
   cut the right edge off.

## Why This Matters

Steps 2 and 4 are the ones that keep a demo from acting on real state:
without the foreign loop claim the tui owns the loop and acts on the
seeded backlog (launches, deletes) against whatever `api_url` says.
Pointing at a fake server alone isn't enough when the binary still has
`gh` and your GitHub login.

## When to Apply

- Screenshots for a PR, README or review of a tui change.
- Checking a view's layout with more rows than your real account has.

## Examples

The `:backlog` / `runs(#12)` / `:runs` screenshots for the backlog and
runs split (PR #174) were made this way.
