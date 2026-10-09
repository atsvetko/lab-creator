package labctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// startBuild builds the requested ref and records the result.
func (s *Server) startBuild(ref string) (*Job, error) {
	return s.mgr.Start("build "+orDefault(ref, s.lab.Ref), func(ctx context.Context, j *Job) error {
		b, err := Build(ctx, j, s.lab, ref)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.built = b
		s.mu.Unlock()
		return nil
	})
}

// startMachine runs a per-machine action.
func (s *Server) startMachine(name, action string) (*Job, error) {
	m := s.lab.Machine(name)
	if m == nil {
		return nil, fmt.Errorf("no such machine %q", name)
	}
	cr := s.snapCreds()
	title := action + " " + name
	return s.mgr.Start(title, func(ctx context.Context, j *Job) error {
		switch action {
		case "create":
			iso, err := answerISO(ctx, j, s.lab, m, cr)
			if err != nil {
				return err
			}
			return createVM(ctx, j, s.lab, m, iso)
		case "start":
			return startVM(ctx, j, m.Name)
		case "stop":
			return stopVM(ctx, j, m.Name)
		case "provision":
			if err := s.provision(ctx, j, m, cr); err != nil {
				return err
			}
			return waitLDAP(ctx, j, m.IP, 10*time.Minute)
		case "snapshot":
			return snapshotVM(ctx, j, m.Name, "vuln-baseline")
		case "reset":
			if err := restoreVM(ctx, j, m.Name, "vuln-baseline"); err != nil {
				return err
			}
			return waitLDAP(ctx, j, m.IP, 6*time.Minute)
		case "scan":
			return s.scan(ctx, j, m, cr)
		default:
			return fmt.Errorf("unknown action %q", action)
		}
	})
}

// startRunAll resets every machine that has a baseline, then scans and verifies it.
func (s *Server) startRunAll() (*Job, error) {
	cr := s.snapCreds()
	return s.mgr.Start("run all", func(ctx context.Context, j *Job) error {
		var failed int
		for i := range s.lab.Machines {
			m := &s.lab.Machines[i]
			j.Logf("=== %s (%s) ===", m.Name, m.Kind)
			if !HasCheckpoint(ctx, m.Name, "vuln-baseline") {
				j.Logf("%s: no baseline checkpoint — skipping (Provision + Snapshot it first)", m.Name)
				continue
			}
			if err := restoreVM(ctx, j, m.Name, "vuln-baseline"); err != nil {
				j.Logf("%s: reset failed: %v", m.Name, err)
				failed++
				continue
			}
			if err := waitLDAP(ctx, j, m.IP, 6*time.Minute); err != nil {
				j.Logf("%s: %v", m.Name, err)
				failed++
				continue
			}
			if err := s.scan(ctx, j, m, cr); err != nil {
				j.Logf("%s: scan failed: %v", m.Name, err)
				failed++
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d machine(s) failed", failed)
		}
		return nil
	})
}

func (s *Server) provision(ctx context.Context, j *Job, m *Machine, cr Creds) error {
	if cr.AdminPassword == "" {
		return fmt.Errorf("set the admin password first (Credentials)")
	}
	switch m.Kind {
	case "windows-dc":
		return provisionWindows(ctx, j, s.lab, m, cr)
	case "alt-dc":
		return provisionAlt(ctx, j, s.lab, m, cr)
	}
	return fmt.Errorf("unknown kind %q", m.Kind)
}

func (s *Server) scan(ctx context.Context, j *Job, m *Machine, cr Creds) error {
	if cr.AuditPassword == "" {
		return fmt.Errorf("set the audit password first (Credentials)")
	}
	s.mu.Lock()
	b := s.built
	s.mu.Unlock()
	if b == nil {
		return fmt.Errorf("build the tool first")
	}
	report, err := deployAndScan(ctx, j, s.lab, b, m, cr)
	if err != nil {
		return err
	}
	pass, verr := verify(ctx, j, s.lab, m, report)
	summary := summarize(j, report)
	s.mu.Lock()
	s.results[m.Name] = MachineResult{Scanned: time.Now(), Pass: pass, Summary: summary, Report: report}
	s.mu.Unlock()
	if verr != nil {
		return fmt.Errorf("verify failed: %w", verr)
	}
	return nil
}

func (s *Server) snapCreds() Creds {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creds
}

// summarize reads a report.json and returns a one-line summary for the UI.
func summarize(j *Job, report string) string {
	b, err := os.ReadFile(report)
	if err != nil {
		return "no report"
	}
	var r struct {
		Checks []json.RawMessage `json:"checks"`
		Counts struct {
			Findings int `json:"findings"`
		} `json:"counts"`
		Score   int `json:"score"`
		Preview int `json:"preview_checks"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "unreadable report"
	}
	line := fmt.Sprintf("%d checks · %d findings · score %d · %d preview",
		len(r.Checks), r.Counts.Findings, r.Score, r.Preview)
	j.Logf("result: %s", line)
	return line
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
