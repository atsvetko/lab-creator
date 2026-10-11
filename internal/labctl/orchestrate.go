package labctl

import (
	"context"
	"fmt"
	"time"
)

// machineAction runs one per-machine step synchronously inside a job. The UI
// and the CLI both go through it, so they behave the same.
func machineAction(ctx context.Context, j *Job, l *Lab, m *Machine, action string, cr Creds) error {
	switch action {
	case "create":
		iso, err := answerISO(ctx, j, l, m, cr)
		if err != nil {
			return err
		}
		err = createVM(ctx, j, l, m, iso)
		forgetHost() // the switch may have just been created
		return err
	case "start":
		return startVM(ctx, j, m.Name)
	case "stop":
		return stopVM(ctx, j, m.Name)
	case "provision":
		p, ok := plugin(m.Kind)
		if !ok || p.Provision == nil {
			return fmt.Errorf("no provisioner for kind %q", m.Kind)
		}
		if err := p.Provision(ctx, j, l, m, cr); err != nil {
			return err
		}
		if p.Scanner {
			return nil
		}
		return waitLDAP(ctx, j, m.IP, 12*time.Minute)
	case "snapshot":
		if m.isScanner() {
			return fmt.Errorf("%s is the scanner; it has no directory to baseline", m.Name)
		}
		return snapshotVM(ctx, j, m.Name, baselineCheckpoint)
	case "reset":
		if m.isScanner() {
			return fmt.Errorf("%s is the scanner; it has no baseline", m.Name)
		}
		if err := restoreVM(ctx, j, m.Name, baselineCheckpoint); err != nil {
			return err
		}
		return waitLDAP(ctx, j, m.IP, 6*time.Minute)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
}

// startMachine runs a per-machine action as a background job, after checking
// the Setup requirements for that action.
func (s *Server) startMachine(name, action string) (*Job, error) {
	m := s.lab.Machine(name)
	if m == nil {
		return nil, fmt.Errorf("no such machine %q", name)
	}
	cr := s.snapCreds()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cs := Checks(ctx, s.lab, cr)
	cancel()
	if err := gate(cs, action, m); err != nil {
		return nil, err
	}
	l := s.lab
	return s.mgr.Start(action+" "+name, func(ctx context.Context, j *Job) error {
		if err := machineAction(ctx, j, l, m, action, cr); err != nil {
			return err
		}
		s.refreshInfo(ctx, j, l, cr, action)
		return nil
	})
}

// startResetAll restores every directory server to its baseline.
func (s *Server) startResetAll() (*Job, error) {
	cr := s.snapCreds()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cs := Checks(ctx, s.lab, cr)
	cancel()
	if err := gate(cs, "reset", nil); err != nil {
		return nil, err
	}
	l := s.lab
	return s.mgr.Start("reset all", func(ctx context.Context, j *Job) error {
		err := resetAll(ctx, j, l)
		s.refreshInfo(ctx, j, l, cr, "reset")
		return err
	})
}

// resetAll restores every machine that has a baseline checkpoint.
func resetAll(ctx context.Context, j *Job, l *Lab) error {
	var failed, done int
	for i := range l.Machines {
		m := &l.Machines[i]
		if m.isScanner() {
			continue
		}
		if !HasCheckpoint(ctx, m.Name, baselineCheckpoint) {
			j.Logf("%s: no baseline checkpoint — skipping (provision and snapshot it first)", m.Name)
			continue
		}
		j.Logf("=== %s (%s) ===", m.Name, m.Kind)
		if err := machineAction(ctx, j, l, m, "reset", Creds{}); err != nil {
			j.Logf("%s: %v", m.Name, err)
			failed++
			continue
		}
		done++
	}
	if failed > 0 {
		return fmt.Errorf("%d machine(s) failed to reset", failed)
	}
	if done == 0 {
		return fmt.Errorf("nothing to reset: no machine has a baseline yet")
	}
	return nil
}

// refreshInfo rewrites lab-info.json after an action that changes what a
// tester would see. Failure to write it is logged, not fatal.
func (s *Server) refreshInfo(ctx context.Context, j *Job, l *Lab, cr Creds, action string) {
	switch action {
	case "provision", "snapshot", "reset":
	default:
		return
	}
	if p, err := WriteInfo(ctx, l, cr); err != nil {
		j.Logf("could not write lab-info.json: %v", err)
	} else {
		j.Logf("lab description: %s", p)
	}
}

func (s *Server) snapCreds() Creds {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creds
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
