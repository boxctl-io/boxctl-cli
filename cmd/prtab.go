package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// openPR shows a pull request in the browser. On a Mac with Google Chrome
// running it reuses one tab -- the one it opened last time, remembered in
// ~/.boxctl/pr-tab -- instead of opening a new tab per PR, so the tui's
// `o` doesn't leave a tab behind on every press. Anywhere else, or if
// that fails, it's `gh pr view --web`, which opens a new tab.
func openPR(repo string, pr int) error {
	url := "https://github.com/" + repo + "/pull/" + strconv.Itoa(pr)
	if chromeRunning() {
		if err := openInChromeTab(url); err == nil {
			return nil
		}
	}
	return exec.Command("gh", "pr", "view", strconv.Itoa(pr), "--repo", repo, "--web").Run()
}

// chromeRunning is checked before any AppleScript names Chrome: telling an
// application that isn't installed asks the user where it is, and one
// that isn't running would be launched.
func chromeRunning() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	return exec.Command("pgrep", "-x", "Google Chrome").Run() == nil
}

// prTabPath is where the reused tab's Chrome id is kept. A variable for tests.
var prTabPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".boxctl", "pr-tab"), nil
}

// chromeTabScript loads argv's URL into the tab whose id is argv's second
// item, if it still exists and is still on GitHub (Chrome's tab ids start
// over when it restarts, so a stale id could name someone else's tab),
// and otherwise into a new tab; it brings that tab forward and returns
// its id.
const chromeTabScript = `on run argv
	set theURL to item 1 of argv
	set tabID to item 2 of argv
	tell application "Google Chrome"
		if tabID is not "" then
			repeat with w in windows
				set i to 0
				repeat with t in tabs of w
					set i to i + 1
					if ((id of t) as text) is tabID and (URL of t) starts with "https://github.com/" then
						set URL of t to theURL
						set active tab index of w to i
						set index of w to 1
						activate
						return tabID
					end if
				end repeat
			end repeat
		end if
		if (count of windows) is 0 then
			make new window
			set t to active tab of front window
			set URL of t to theURL
		else
			tell front window to set t to make new tab with properties {URL:theURL}
		end if
		activate
		return ((id of t) as text)
	end tell
end run`

func openInChromeTab(url string) error {
	p, err := prTabPath()
	if err != nil {
		return err
	}
	prev, _ := os.ReadFile(p)
	out, err := exec.Command("osascript", "-e", chromeTabScript, url, strings.TrimSpace(string(prev))).Output()
	if err != nil {
		return err
	}
	if id := strings.TrimSpace(string(out)); id != "" && id != strings.TrimSpace(string(prev)) {
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		_ = os.WriteFile(p, []byte(id+"\n"), 0o600)
	}
	return nil
}
