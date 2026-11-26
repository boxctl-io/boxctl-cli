package cmd

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/boxctl-io/boxctl-cli/internal/client"
	"github.com/gorilla/websocket"
)

// dashboardFixture serves a dashboard on a real loopback port (its Host
// check needs the port) in front of a fake boxctl-vms listing
// tuiFixture's boxes.
func dashboardFixture(t *testing.T) (*dashboard, string, *http.Client) {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/vms":
			_ = json.NewEncoder(w).Encode(tuiFixture())
		case r.Method == http.MethodPost && r.URL.Path == "/api/vms/web/pause":
			_ = json.NewEncoder(w).Encode(client.VM{Name: "web", State: "paused"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d, err := newDashboard(client.New(api.URL, "tok"), ln.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: d}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return d, d.origin, hc
}

func get(t *testing.T, hc *http.Client, url string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func TestDashboardGate(t *testing.T) {
	d, base, hc := dashboardFixture(t)
	cookie := dashboardCookie + "=" + d.secret

	if res := get(t, hc, base+"/api/boxes", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("no cookie: %d, want 401", res.StatusCode)
	}
	if res := get(t, hc, base+"/?k=wrong", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("wrong link: %d, want 403", res.StatusCode)
	}
	res := get(t, hc, base+"/?k="+d.secret, nil)
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Set-Cookie"), "SameSite=Strict") {
		t.Errorf("link: %d %q, want a redirect setting a strict cookie", res.StatusCode, res.Header.Get("Set-Cookie"))
	}
	if res := get(t, hc, base+"/", map[string]string{"Cookie": cookie}); res.StatusCode != http.StatusOK {
		t.Errorf("page: %d", res.StatusCode)
	}
	// DNS rebinding: the right cookie, some other name for 127.0.0.1.
	req, _ := http.NewRequest(http.MethodGet, base+"/api/boxes", nil)
	req.Host = "evil.example:80"
	req.Header.Set("Cookie", cookie)
	if res, err := hc.Do(req); err != nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Host: %v %v, want 403", res, err)
	}
	if res := get(t, hc, base+"/api/boxes", map[string]string{"Cookie": cookie, "Origin": "https://evil.example"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Origin: %d, want 403", res.StatusCode)
	}

	res = get(t, hc, base+"/api/boxes", map[string]string{"Cookie": cookie, "Origin": base})
	var boxes []dashboardBox
	if err := json.NewDecoder(res.Body).Decode(&boxes); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range boxes {
		names = append(names, b.Name)
	}
	if got, want := strings.Join(names, " "), "booting goal-box web old-paused"; got != want {
		t.Errorf("boxes = %q, want %q (ls's order)", got, want)
	}
	for _, b := range boxes {
		if b.Name == "goal-box" && (!b.Unfinished || !b.Unattended || b.Run != "claude code: running") {
			t.Errorf("goal-box = %+v, want an unfinished goal run", b)
		}
	}

	req, _ = http.NewRequest(http.MethodPost, base+"/api/boxes/web/pause", nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", base)
	if res, err := hc.Do(req); err != nil || res.StatusCode != http.StatusOK {
		t.Errorf("pause: %v %v", res, err)
	}
}

func TestDashboardTerminalRelay(t *testing.T) {
	d, base, _ := dashboardFixture(t)
	// The box's end: echo every message back, prefixed.
	box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(mt, append([]byte("box:"), msg...))
		}
	}))
	defer box.Close()
	var gotName string
	var gotSession int
	d.dialTerminal = func(ctx context.Context, name string, session int) (*websocket.Conn, error) {
		gotName, gotSession = name, session
		c, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(box.URL, "http"), nil)
		return c, err
	}

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/ws/terminal/web?session=2"
	hdr := http.Header{"Cookie": {dashboardCookie + "=" + d.secret}, "Origin": {"https://evil.example"}}
	if _, res, err := websocket.DefaultDialer.Dial(wsURL, hdr); err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin upgrade: %v, want refused", err)
	}
	hdr.Set("Origin", base)
	c, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(msg) != "box:ls\r" || gotName != "web" || gotSession != 2 {
		t.Errorf("relayed %q to %s session %d", msg, gotName, gotSession)
	}
}

func TestDashboardPageEmbedded(t *testing.T) {
	if !strings.Contains(string(dashboardHTML), "/ws/terminal/") {
		t.Error("dashboard.html isn't the page")
	}
}
