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
)

//go:embed web/*
var webFS embed.FS

// MachineResult is the outcome of the last scan of a machine.
type MachineResult struct {
	Scanned time.Time `json:"scanned"`
	Pass    bool      `json:"pass"`
	Summary string    `json:"summary"`
	Report  string    `json:"report"`
}

// Server is the lab control panel.
type Server struct {
	lab   *Lab
	mgr   *Manager
	token string
	addr  string
	stop  func()

	mu      sync.Mutex
	built   *Built
	creds   Creds
	results map[string]MachineResult
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
		addr: ln.Addr().String(), results: map[string]MachineResult{},
		creds: Creds{AuditUser: "audit"}}
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
	api("POST /api/build", s.handleBuild)
	api("POST /api/machine", s.handleMachine)
	api("POST /api/run-all", s.handleRunAll)
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
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	hostOK, hostMsg := HostOK()

	s.mu.Lock()
	built := s.built
	credsSet := s.creds.AdminPassword != "" || s.creds.AuditPassword != ""
	results := map[string]MachineResult{}
	for k, v := range s.results {
		results[k] = v
	}
	s.mu.Unlock()

	type mstate struct {
		Machine
		VMState     string         `json:"vm_state"`
		HasBaseline bool           `json:"has_baseline"`
		Result      *MachineResult `json:"result,omitempty"`
	}
	ms := make([]mstate, 0, len(s.lab.Machines))
	for _, m := range s.lab.Machines {
		st := ""
		base := false
		if hostOK {
			st = VMState(ctx, m.Name)
			if st != "" {
				base = HasCheckpoint(ctx, m.Name, "vuln-baseline")
			}
		}
		x := mstate{Machine: m, VMState: st, HasBaseline: base}
		if rr, ok := results[m.Name]; ok {
			x.Result = &rr
		}
		ms = append(ms, x)
	}

	var job any
	if j := s.mgr.Current(); j != nil {
		job = map[string]any{"id": j.ID, "title": j.Title, "status": j.Status}
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "host_ok": hostOK, "host_msg": hostMsg,
		"lab": s.lab, "built": built, "creds_set": credsSet,
		"machines": ms, "busy": s.mgr.Busy(), "job": job,
	})
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
	// Preserve the path; take the editable fields from the request.
	nl.path = s.lab.path
	if err := nl.Save(); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.lab = &nl
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleBuild(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref string `json:"ref"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	j, err := s.startBuild(req.Ref)
	s.jobReply(w, j, err)
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

func (s *Server) handleRunAll(w http.ResponseWriter, r *http.Request) {
	j, err := s.startRunAll()
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
