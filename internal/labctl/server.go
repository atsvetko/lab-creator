package labctl

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/atsvetko/lab-creator/content"
)

func contentVersion() string { return content.Version }

//go:embed web/*
var webFS embed.FS

// Server is the lab control panel.
type Server struct {
	lab   *Lab
	mgr   *Manager
	token string
	addr  string
	stop  func()

	mu    sync.Mutex
	creds Creds
}

// Serve starts the control panel on 127.0.0.1 and returns its URL (with the
// one-time token). The caller prints it and, optionally, opens a browser.
func Serve(lab *Lab) (*Server, string, error) {
	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		return nil, "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	stop := make(chan struct{})
	s := &Server{lab: lab, mgr: NewManager(), token: hex.EncodeToString(tok),
		addr: ln.Addr().String(), creds: Creds{AuditUser: "audit"}}
	s.stop = func() { close(stop) }
	srv := &http.Server{Handler: s.routes()}
	go func() { _ = srv.Serve(ln) }()
	go func() { <-stop; _ = srv.Close() }()
	return s, "http://" + s.addr + "/?t=" + s.token, nil
}

func (s *Server) routes() http.Handler {
	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(sub))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenOK(r) {
			http.Error(w, "Open the address printed in the console (it carries a one-time token).", http.StatusForbidden)
			return
		}
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
	mux.Handle("GET /app.js", files)
	mux.Handle("GET /app.css", files)

	api := func(p string, h http.HandlerFunc) {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			if !s.tokenOK(r) {
				writeJSON(w, 403, map[string]any{"ok": false, "error": "missing or wrong token"})
				return
			}
			h(w, r)
		})
	}
	api("GET /api/state", s.handleState)
	api("POST /api/creds", s.handleCreds)
	api("POST /api/config", s.handleConfig)
	api("POST /api/machine", s.handleMachine)
	api("POST /api/reset-all", s.handleResetAll)
	api("GET /api/lab-info", s.handleLabInfo)
	api("GET /api/logs", s.handleLogs)
	api("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
		go func() { time.Sleep(200 * time.Millisecond); s.stop() }()
	})
	return mux
}

func (s *Server) tokenOK(r *http.Request) bool {
	t := r.URL.Query().Get("t")
	if t == "" {
		t = r.Header.Get("X-Lab-Token")
	}
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1
}

// --- handlers ---------------------------------------------------------------

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	hostOK, hostMsg := HostOK()

	s.mu.Lock()
	cr := s.creds
	s.mu.Unlock()
	checks := Checks(ctx, s.lab, cr)

	type mstate struct {
		Machine
		VMState     string `json:"vm_state"`
		HasBaseline bool   `json:"has_baseline"`
		Scanner     bool   `json:"scanner"`
	}
	ms := make([]mstate, 0, len(s.lab.Machines))
	for _, m := range s.lab.Machines {
		st := ""
		base := false
		if hostOK {
			st = VMState(ctx, m.Name)
			if st != "" && !m.isScanner() {
				base = HasCheckpoint(ctx, m.Name, baselineCheckpoint)
			}
		}
		ms = append(ms, mstate{Machine: m, VMState: st, HasBaseline: base, Scanner: m.isScanner()})
	}

	var job any
	if j := s.mgr.Current(); j != nil {
		job = map[string]any{"id": j.ID, "title": j.Title, "status": j.Status}
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "host_ok": hostOK, "host_msg": hostMsg,
		"lab": s.lab, "lab_path": s.lab.Path(), "saved": s.lab.Saved(), "warnings": s.lab.Warnings(),
		"checks": checks, "ready": Ready(checks),
		"creds_set": cr.AdminPassword != "" && cr.AuditPassword != "", "audit_user": cr.auditUser(),
		"machines": ms, "busy": s.mgr.Busy(), "job": job, "kinds": Kinds(),
		"content_version": contentVersion(),
	})
}

func (s *Server) handleLabInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	w.Header().Set("Content-Disposition", `attachment; filename="lab-info.json"`)
	writeJSON(w, 200, BuildInfo(ctx, s.lab, s.snapCreds()))
}

func (s *Server) handleCreds(w http.ResponseWriter, r *http.Request) {
	var c Creds
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "bad json"})
		return
	}
	s.mu.Lock()
	if c.AdminPassword != "" {
		s.creds.AdminPassword = c.AdminPassword
	}
	if c.AuditPassword != "" {
		s.creds.AuditPassword = c.AuditPassword
	}
	if c.AuditUser != "" {
		s.creds.AuditUser = c.AuditUser
	}
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	var nl Lab
	if err := json.NewDecoder(r.Body).Decode(&nl); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "bad json"})
		return
	}
	if s.mgr.Busy() {
		writeJSON(w, 409, map[string]any{"ok": false, "error": "an action is running; save the config when it finishes"})
		return
	}
	// Preserve the path; take the editable fields from the request.
	nl.path = s.lab.path
	if err := nl.Save(); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.lab = &nl
	forgetHost() // the switch name may have changed
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleMachine(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, Action string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "bad json"})
		return
	}
	j, err := s.startMachine(req.Name, req.Action)
	s.jobReply(w, j, err)
}

func (s *Server) handleResetAll(w http.ResponseWriter, r *http.Request) {
	j, err := s.startResetAll()
	s.jobReply(w, j, err)
}

func (s *Server) jobReply(w http.ResponseWriter, j *Job, err error) {
	if err != nil {
		writeJSON(w, 409, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "job": j.ID})
}

// handleLogs streams a job's log over Server-Sent Events.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job")
	j := s.mgr.Get(id)
	if j == nil {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	hist, ch, cancel := j.Subscribe()
	defer cancel()
	for _, line := range hist {
		writeSSE(w, "log", line)
	}
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-ch:
			if !ok {
				writeSSE(w, "done", j.Status)
				fl.Flush()
				return
			}
			writeSSE(w, "log", line)
			fl.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, jsonLine(data))
}

func jsonLine(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
