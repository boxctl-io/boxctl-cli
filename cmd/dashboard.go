package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/boxctl-io/boxctl-cli/internal/client"
	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

// `boxctl dashboard`: the tui's boxes app (`:boxes`, `:urls`, `:images`,
// `:sizes`, `:backups`) as a web page served from this machine. The page
// (dashboard.html, embedded) calls a small JSON API here, which makes the
// same client calls the tui does with the saved personal token -- the
// token never reaches the browser. A terminal is the same /ws/terminal
// relay `boxctl ssh` speaks, proxied: the browser's xterm.js talks to this
// server, which mints the ticket and dials boxctl-vms.
//
// It listens on 127.0.0.1 only, and every request needs the session
// secret: the opened URL carries it once (?k=), which sets a SameSite
// strict cookie. The Host header must be the loopback address (no DNS
// rebinding), and a request with an Origin must come from the page.

//go:embed dashboard.html
var dashboardHTML []byte

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Manage your boxes from a web page served on this machine",
	Long: `Serves a dashboard for your boxes on 127.0.0.1 and opens it in your
browser: the boxes (live, refreshed every 5s), their public URLs, images,
sizes and backups -- create, pause, resume, keep, expose, delete, restore,
a --task run's log, and a terminal on a box in the page itself.

It uses this CLI's login; the token stays here. Only this machine can
reach it, and only with the link it prints. Ctrl-C stops it.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		port, _ := cmd.Flags().GetInt("port")
		noBrowser, _ := cmd.Flags().GetBool("no-browser")
		return runDashboard(cmd.Context(), port, noBrowser)
	},
}

func init() {
	dashboardCmd.Flags().Int("port", 0, "port to listen on (default: a free one)")
	dashboardCmd.Flags().Bool("no-browser", false, "print the URL instead of opening it")
	rootCmd.AddCommand(dashboardCmd)
}

func runDashboard(ctx context.Context, port int, noBrowser bool) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("listening: %w", err)
	}
	client.QuietProgress() // Restore's spinner would write over our log lines
	d, err := newDashboard(newClient(), ln.Addr().(*net.TCPAddr).Port)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: d, ReadHeaderTimeout: 10 * time.Second}

	link := d.origin + "/?k=" + d.secret
	fmt.Fprintf(os.Stderr, "boxctl dashboard: %s\n", link)
	if !noBrowser {
		if err := openBrowser(link); err != nil {
			fmt.Fprintf(os.Stderr, "Couldn't open a browser (%v); open the link above.\n", err)
		}
	}
	fmt.Fprintln(os.Stderr, "Ctrl-C stops it.")

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	return nil
}

// dashboard is the local server: the page, its API and the terminal
// proxy, all behind the session secret.
type dashboard struct {
	c      *client.Client
	secret string
	origin string // http://127.0.0.1:<port>
	hosts  map[string]bool
	mux    *http.ServeMux
	// dialTerminal opens the box's terminal WebSocket upstream; a
	// variable so tests needn't stand up boxctl-vms's ticket flow.
	dialTerminal func(ctx context.Context, name string, session int) (*websocket.Conn, error)
}

const dashboardCookie = "boxctl_dashboard"

func newDashboard(c *client.Client, port int) (*dashboard, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	p := strconv.Itoa(port)
	d := &dashboard{
		c:      c,
		secret: hex.EncodeToString(b),
		origin: "http://127.0.0.1:" + p,
		hosts:  map[string]bool{"127.0.0.1:" + p: true, "localhost:" + p: true},
		mux:    http.NewServeMux(),
	}
	d.dialTerminal = d.dialUpstreamTerminal
	m := d.mux
	m.HandleFunc("GET /{$}", d.page)
	m.HandleFunc("GET /api/info", d.info)
	m.HandleFunc("GET /api/boxes", d.boxes)
	m.HandleFunc("POST /api/boxes", d.create)
	m.HandleFunc("DELETE /api/boxes/{name}", d.destroy)
	m.HandleFunc("POST /api/boxes/{name}/{action}", d.boxAction)
	m.HandleFunc("DELETE /api/boxes/{name}/ingress/{port}", d.unexpose)
	m.HandleFunc("GET /api/boxes/{name}/logs", d.logs)
	m.HandleFunc("GET /api/images", d.images)
	m.HandleFunc("GET /api/sizes", d.sizes)
	m.HandleFunc("GET /api/backups", d.backups)
	m.HandleFunc("POST /api/backups/{id}/restore", d.restore)
	m.HandleFunc("GET /ws/terminal/{name}", d.terminal)
	return d, nil
}

func (d *dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !d.hosts[r.Host] {
		http.Error(w, "unexpected Host", http.StatusForbidden)
		return
	}
	if o := r.Header.Get("Origin"); o != "" && o != d.origin && o != strings.Replace(d.origin, "127.0.0.1", "localhost", 1) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	// The opened link: trade ?k= for the cookie, and drop it from the
	// address bar (and so from history and screenshots).
	if k := r.URL.Query().Get("k"); k != "" && r.URL.Path == "/" {
		if !d.secretOK(k) {
			http.Error(w, "wrong or expired dashboard link; run boxctl dashboard again", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: dashboardCookie, Value: d.secret, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	ck, err := r.Cookie(dashboardCookie)
	if err != nil || !d.secretOK(ck.Value) {
		http.Error(w, "open the link boxctl dashboard printed", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	d.mux.ServeHTTP(w, r)
}

func (d *dashboard) secretOK(s string) bool {
	return subtle.ConstantTimeCompare([]byte(s), []byte(d.secret)) == 1
}

func (d *dashboard) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	_, _ = w.Write(dashboardHTML)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// readBody decodes a small JSON request body into v.
func readBody(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(v); err != nil {
		return fmt.Errorf("bad request body: %w", err)
	}
	return nil
}

func (d *dashboard) info(w http.ResponseWriter, r *http.Request) {
	version, _, _ := versionInfo()
	writeJSON(w, http.StatusOK, map[string]string{
		"api_url":   d.c.BaseURL(),
		"dashboard": dashboardBase(),
		"version":   version,
	})
}

// dashboardBox is a box as the page shows it: the API's own fields plus
// the columns ls and the tui compute from them, so the page needn't know
// the label conventions.
type dashboardBox struct {
	client.VM
	Run        string   `json:"run"`
	Project    string   `json:"project"`
	Branch     string   `json:"branch"`
	PR         string   `json:"pr"`
	SizeLabel  string   `json:"size_label"`
	Age        string   `json:"age"`
	Rank       int      `json:"rank"`
	Unfinished bool     `json:"unfinished"` // a --task run that isn't done: rm's second warning
	Unattended bool     `json:"unattended"` // an unattended run: has a log for `logs`
	Describe   []string `json:"describe"`
}

func (d *dashboard) boxes(w http.ResponseWriter, r *http.Request) {
	vms, err := d.c.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	sortBoxes(vms)
	out := make([]dashboardBox, 0, len(vms))
	for _, vm := range vms {
		out = append(out, dashboardBox{
			VM:         vm,
			Run:        runText(vm),
			Project:    projectText(vm),
			Branch:     branchText(vm),
			PR:         prText(vm),
			SizeLabel:  sizeLabel(vm),
			Age:        age(vm.CreatedAt),
			Rank:       stateRank(vm.State),
			Unfinished: len(filterUnfinished([]client.VM{vm}, nil)) > 0,
			Unattended: unattendedBox(vm),
			Describe:   describeBox(vm),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *dashboard) create(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, Image, Size string }
	if err := readBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), createTimeoutTUI)
	defer cancel()
	name := strings.TrimSpace(req.Name)
	if name == "" {
		n, err := newBoxName(ctx, "", "")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		name = n.box()
	}
	image := req.Image
	if image == "" {
		image = defaultImage
	}
	vm, err := d.c.Create(ctx, name, "", image, req.Size, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, vm)
}

func (d *dashboard) destroy(w http.ResponseWriter, r *http.Request) {
	if err := d.c.Destroy(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

// boxAction is pause, resume, keep and expose: one client call each.
func (d *dashboard) boxAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	var (
		v   any
		err error
	)
	switch r.PathValue("action") {
	case "pause":
		v, err = d.c.Pause(ctx, name)
	case "resume":
		v, err = d.c.Resume(ctx, name)
	case "keep":
		v, err = d.c.Keep(ctx, name)
	case "expose":
		var req struct {
			Port int `json:"port"`
		}
		if err := readBody(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if req.Port < 1 || req.Port > 65535 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("%d is not a port", req.Port))
			return
		}
		v, err = d.c.Expose(ctx, name, req.Port, client.ExposeOptions{})
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (d *dashboard) unexpose(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a port", r.PathValue("port")))
		return
	}
	if err := d.c.Unexpose(r.Context(), r.PathValue("name"), port); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

// logs is the tui's `l`: a --task run's log, read over Exec and run
// through the same formatter as `boxctl claude logs`.
func (d *dashboard) logs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res, err := d.c.Exec(ctx, r.PathValue("name"), logsScript(false), time.Minute)
	if err == nil && res.Error != "" {
		err = errors.New(res.Error)
	}
	if err == nil && res.ExitCode != 0 {
		err = errors.New(strings.TrimSpace(res.Stderr))
		if err.Error() == "" {
			err = fmt.Errorf("exit status %d", res.ExitCode)
		}
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	var buf bytes.Buffer
	fm := &logFormatter{w: &buf}
	_, _ = fm.Write([]byte(res.Stdout))
	_ = fm.Flush()
	writeJSON(w, http.StatusOK, map[string]string{"log": buf.String()})
}

func (d *dashboard) images(w http.ResponseWriter, r *http.Request) {
	images, err := d.c.ListImages(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": images, "default": defaultImage})
}

func (d *dashboard) sizes(w http.ResponseWriter, r *http.Request) {
	sizes, err := d.c.ListSizes(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, sizes)
}

func (d *dashboard) backups(w http.ResponseWriter, r *http.Request) {
	backups, err := d.c.ListBackups(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, backups)
}

func (d *dashboard) restore(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, Size string }
	if err := readBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("the new box needs a name"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	vm, notice, err := d.c.Restore(ctx, r.PathValue("id"), strings.TrimSpace(req.Name), strings.TrimSpace(req.Size))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vm": vm, "notice": notice.Text()})
}

var dashboardUpgrader = websocket.Upgrader{
	// ServeHTTP has already refused any Origin but the page's own.
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (d *dashboard) dialUpstreamTerminal(ctx context.Context, name string, session int) (*websocket.Conn, error) {
	ticket, vmID, err := d.c.MintTerminalTicket(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("opening a terminal on %s: %w", name, err)
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, terminalWSURL(d.c.BaseURL(), vmID, ticket, session), nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to terminal: %w", err)
	}
	return conn, nil
}

// terminal relays the page's xterm.js to the box's terminal, message for
// message. The page already speaks the protocol -- keystrokes as binary
// messages, a resize as resizeMarker + JSON -- so nothing is translated.
// ?session=N (1-5) is ssh's --session.
func (d *dashboard) terminal(w http.ResponseWriter, r *http.Request) {
	session, _ := strconv.Atoi(r.URL.Query().Get("session"))
	if session < 0 || session > 5 {
		session = 0
	}
	page, err := dashboardUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = page.Close() }()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	box, err := d.dialTerminal(ctx, r.PathValue("name"), session)
	cancel()
	if err != nil {
		_ = page.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, truncate(err.Error(), 60)), time.Now().Add(time.Second))
		return
	}
	defer func() { _ = box.Close() }()
	relayWS(page, box)
}

// relayWS copies messages both ways until either side ends.
func relayWS(a, b *websocket.Conn) {
	done := make(chan struct{}, 2)
	pipe := func(dst, src *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			t, msg, err := src.ReadMessage()
			if err != nil {
				return
			}
			if err := dst.WriteMessage(t, msg); err != nil {
				return
			}
		}
	}
	go pipe(a, b)
	go pipe(b, a)
	<-done
	// WriteControl, not WriteMessage: the other pipe may still be
	// writing, and only WriteControl may run alongside it.
	bye := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	deadline := time.Now().Add(time.Second)
	_ = a.WriteControl(websocket.CloseMessage, bye, deadline)
	_ = b.WriteControl(websocket.CloseMessage, bye, deadline)
}
