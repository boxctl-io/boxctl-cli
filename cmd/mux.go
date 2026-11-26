package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// A multiplexer keeps an agent's session running on the box between
// attaches -- across a detach, a dropped connection, a --detach start.
// herdr (herdr.dev) is the default; --mux tmux picks tmux.
// herdr is a terminal workspace manager made for coding agents, which
// tells a working one from a blocked one from an idle one, and shows
// every workspace on the box in one window. Which one a box uses is recorded on it
// (muxMarker) when a session starts, so later runs -- attaching again,
// claude ls -- look for the session where it is without being told.
type multiplexer interface {
	name() string
	// detachKeys is what detaches from an attached session.
	detachKeys() string
	// setup is shell that installs the multiplexer on the box if it's
	// missing.
	setup() string
	// start is shell that starts s's agent with line as its command, or
	// attaches to it if it's running already; detached starts it without
	// attaching (--detach, the goal runner).
	start(s workspace, line string, detached bool) string
	// running is shell that exits 0 if s's agent is running.
	running(s workspace) string
	// list is shell printing the sessions, for parseList.
	list() string
	// parseList maps each running session's name to what claude ls shows
	// for it: when it last showed activity, or what its agent is doing.
	parseList(out string) map[string]string
}

const muxEnv = "BOXCTL_MUX"

var (
	// muxMarker on the box records which multiplexer its sessions run in.
	muxMarker = "/root/.boxctl/mux"
	// muxRunDir on the box, a tmpfs, holds a session's secrets and its
	// command while it starts.
	muxRunDir = "/run/boxctl"

	// mux is the running command's multiplexer: --mux, or the box's.
	mux multiplexer = tmuxMux{}
	// muxArg is --mux; muxExplicit says it was given, so it beats the
	// box's marker.
	muxArg      string
	muxExplicit bool
)

// muxNamed is the multiplexer called name.
func muxNamed(name string) (multiplexer, error) {
	switch name {
	case "tmux":
		return tmuxMux{}, nil
	case "herdr":
		return herdrMux{}, nil
	}
	return nil, fmt.Errorf("--mux must be tmux or herdr, not %q", name)
}

// defaultMux is a's --mux default: BOXCTL_MUX, else the agent's own
// (herdr for claude).
func (a *boxAgent) defaultMux() string {
	if v := os.Getenv(muxEnv); v != "" {
		return v
	}
	if a.mux != "" {
		return a.mux
	}
	return "tmux"
}

// muxMarkScript is shell that records name as the box's multiplexer.
func muxMarkScript(name string) string {
	return "mkdir -p " + shellQuote(path.Dir(muxMarker)) + " && printf '%s' " + name + " >" + shellQuote(muxMarker)
}

// --- tmux -----------------------------------------------------------------

type tmuxMux struct{}

func (tmuxMux) name() string       { return "tmux" }
func (tmuxMux) detachKeys() string { return "Ctrl-b d" }

// setup: the claude-agent image has tmux; other images get it from apt.
func (tmuxMux) setup() string {
	return `command -v tmux >/dev/null || { echo "Installing tmux..." >&2; apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq tmux >/dev/null; }`
}

// start: one tmux session per agent session. -A attaches to it if it's
// running already, -d starts it without attaching. -u: the box has no
// locale set, and the agent's interface is Unicode.
func (tmuxMux) start(s workspace, line string, detached bool) string {
	mode := "-A"
	if detached {
		mode = "-d"
	}
	return muxMarkScript("tmux") + fmt.Sprintf("; tmux -u new-session %s -s %s -c %s %s",
		mode, s.session, shellQuote(s.dir), shellQuote(line))
}

// running: =name is exact. A bare -t name falls back to a prefix match,
// so "claude" would find "claude-auth" and take the main session for
// running.
func (tmuxMux) running(s workspace) string {
	return "tmux has-session -t =" + s.session + " 2>/dev/null"
}

func (tmuxMux) list() string {
	return "tmux ls -F '#{session_name} #{session_activity}' 2>/dev/null || true"
}

// parseList: each session's last activity, as "5m ago".
func (tmuxMux) parseList(out string) map[string]string {
	active := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, at, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(at, 10, 64); err == nil {
			active[name] = ago(time.Unix(n, 0))
		}
	}
	return active
}

// --- herdr ----------------------------------------------------------------

// herdr runs one server per box (its default session), and each agent
// session is a workspace of it labeled with the session's name (claude,
// claude-auth, claude-goal-<id>): an attached herdr shows every run on
// the box, with what its agent is doing, and claude ls reads them all
// from one `herdr workspace list`. Verified against herdr 0.9.5.
type herdrMux struct{}

func (herdrMux) name() string       { return "herdr" }
func (herdrMux) detachKeys() string { return "Ctrl-b q" }

// setup installs herdr with its own installer, into /usr/local/bin: the
// installer's default, ~/.local/bin, isn't on a non-login shell's PATH.
// It also turns off herdr's first-run screen (a welcome and an
// integrations picker over every attach until someone dismisses it --
// boxctl installs the agent's integration itself): `onboarding = false`
// at the top of config.toml, unless the file already says either way.
// A server already running is told to reload it.
func (herdrMux) setup() string {
	return `command -v herdr >/dev/null || { echo "Installing herdr..." >&2; curl -fsSL https://herdr.dev/install.sh | HERDR_INSTALL_DIR=/usr/local/bin sh >&2; }
hc="$HOME/.config/herdr/config.toml"
if ! grep -qsE '^[[:space:]]*onboarding[[:space:]]*=' "$hc"; then
  mkdir -p "$(dirname "$hc")"
  { printf '%s\n' '# boxctl: no first-run screen on a box' 'onboarding = false'; cat "$hc" 2>/dev/null || true; } >"$hc.tmp" && mv "$hc.tmp" "$hc"
  herdr server reload-config >/dev/null 2>&1 || true
fi`
}

// start starts the server headless if it isn't running (`herdr
// workspace list` fails without one; `herdr status` exits 0 either way),
// with the whole subshell's output redirected, not only the server's: a
// subshell left waiting on it would hold the ssh session's output open,
// and the start -- a goal's launch -- would never return.
// A server started afresh restores its last layout -- shells in the old
// sessions' directories, not the sessions, which died with it -- so
// those workspaces are closed first, or an old session's label would
// pass for a running one. Then it makes the session's workspace unless
// it exists: its root pane's
// shell starts in the session's directory, and line is typed into that
// shell by way of a file -- a typed line is capped by the tty (4 KB on
// Linux), and a --prompt isn't. Attaching is focusing the workspace and
// running the client. The integration (a Claude Code hook that reports
// the session to herdr, for restoring it) is installed with the
// workspace, by which time the Claude configuration is on the box.
//
// A pane whose process ends is closed, and an attached client then opens
// a plain shell workspace instead of ending. An attached session with no
// shell after the agent (an ephemeral run, whose end waits for the attach
// to return) therefore stops the server when the agent exits, which ends
// the client; a session that keeps a shell (shellAfter) is left to it,
// and so is a detached one, which nothing waits for.
func (herdrMux) start(s workspace, line string, detached bool) string {
	cmdFile := muxRunDir + "/" + s.session + ".cmd"
	pane := "exec sh " + shellQuote(cmdFile)
	if !detached && !s.shellAfter {
		pane = "sh " + shellQuote(cmdFile) + "; herdr server stop"
	}
	script := muxMarkScript("herdr") + `
if ! herdr workspace list >/dev/null 2>&1; then
  (cd / && exec setsid herdr server) >/dev/null 2>&1 </dev/null &
  n=0; until herdr workspace list >/dev/null 2>&1; do n=$((n+1)); [ "$n" -lt 100 ] || { echo "herdr's server didn't start" >&2; exit 1; }; sleep 0.1; done
  for w in $(herdr workspace list 2>/dev/null | grep -o '"workspace_id":"[^"]*"' | sed 's/.*:"//; s/"$//'); do herdr workspace close "$w" >/dev/null 2>&1; done
fi
ws=$(herdr workspace list 2>/dev/null | grep -o ` + shellQuote(`"label":"`+s.session+`"[^}]*"workspace_id":"[^"]*"`) + ` | sed 's/.*"workspace_id":"//; s/"$//')
if [ -n "$ws" ]; then herdr workspace focus "$ws" >/dev/null || exit 1
else
  herdr integration install ` + agent.name + ` >/dev/null 2>&1 || true
  (umask 077 && mkdir -p ` + shellQuote(muxRunDir) + ` && printf '%s\n' ` + shellQuote(line) + ` >` + shellQuote(cmdFile) + `) || exit 1
  p=$(herdr workspace create --cwd ` + shellQuote(s.dir) + ` --label ` + shellQuote(s.session) + ` --focus | sed -n 's/.*"root_pane":{[^}]*"pane_id":"\([^"]*\)".*/\1/p')
  [ -n "$p" ] || { echo "couldn't make a herdr workspace for ` + s.session + `" >&2; exit 1; }
  herdr pane run "$p" ` + shellQuote(pane) + ` >/dev/null || exit 1
fi`
	if !detached {
		script += "\nexec herdr"
	}
	return script
}

// running: the session's workspace exists. A workspace goes when its
// pane's process ends, so one still listed is one still running.
func (herdrMux) running(s workspace) string {
	return "herdr workspace list 2>/dev/null | grep -q " + shellQuote(`"label":"`+s.session+`"`)
}

func (herdrMux) list() string {
	return "herdr workspace list 2>/dev/null || true"
}

// parseList: each workspace's agent status -- working, blocked, idle,
// done, or unknown for one herdr can't classify.
func (herdrMux) parseList(out string) map[string]string {
	var v struct {
		Result struct {
			Workspaces []struct {
				Label       string `json:"label"`
				AgentStatus string `json:"agent_status"`
			} `json:"workspaces"`
		} `json:"result"`
	}
	active := map[string]string{}
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &v) != nil {
		return active
	}
	for _, w := range v.Result.Workspaces {
		status := w.AgentStatus
		if status == "" {
			status = "-"
		}
		active[w.Label] = status
	}
	return active
}
