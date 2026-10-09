package labctl

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// redact hides anything that looks like a secret in a logged command line.
func redact(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		low := strings.ToLower(a)
		switch {
		case strings.Contains(low, "password"), strings.Contains(low, "secret"):
			out[i] = "***"
		default:
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}

// Job is one orchestration action (build, create, provision, scan…), with a
// streamed log the UI tails over SSE. Only one job runs at a time because the
// lab VMs are shared hardware.
type Job struct {
	ID     string
	Title  string
	Status string // running | ok | failed
	Start  time.Time
	End    time.Time

	mu   sync.Mutex
	logs []string
	subs map[chan string]struct{}
	done bool
}

func (j *Job) append(line string) {
	j.mu.Lock()
	j.logs = append(j.logs, line)
	for ch := range j.subs {
		select {
		case ch <- line:
		default: // slow subscriber: drop rather than block the job
		}
	}
	j.mu.Unlock()
}

// Subscribe returns the log so far and a channel of subsequent lines; call the
// returned cancel to unsubscribe. A closed channel means the job finished.
func (j *Job) Subscribe() ([]string, chan string, func()) {
	j.mu.Lock()
	defer j.mu.Unlock()
	hist := append([]string(nil), j.logs...)
	ch := make(chan string, 256)
	if j.done {
		close(ch)
		return hist, ch, func() {}
	}
	if j.subs == nil {
		j.subs = map[chan string]struct{}{}
	}
	j.subs[ch] = struct{}{}
	return hist, ch, func() {
		j.mu.Lock()
		delete(j.subs, ch)
		j.mu.Unlock()
	}
}

func (j *Job) finish(err error) {
	j.mu.Lock()
	j.End = time.Now()
	if err != nil {
		j.Status = "failed"
		j.logs = append(j.logs, "ERROR: "+err.Error())
	} else {
		j.Status = "ok"
	}
	j.done = true
	for ch := range j.subs {
		close(ch)
	}
	j.subs = nil
	j.mu.Unlock()
}

// Logf adds a labctl-level line (distinct from command output).
func (j *Job) Logf(f string, a ...any) { j.append("» " + fmt.Sprintf(f, a...)) }

// Manager runs one job at a time and keeps the most recent for the UI to tail.
type Manager struct {
	mu   sync.Mutex
	cur  *Job
	last *Job
	n    int
}

func NewManager() *Manager { return &Manager{} }

// Busy reports whether a job is running.
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur != nil
}

// Current returns the running job, or the last finished one.
func (m *Manager) Current() *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil {
		return m.cur
	}
	return m.last
}

// Get returns a job by id (current or last).
func (m *Manager) Get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range []*Job{m.cur, m.last} {
		if j != nil && j.ID == id {
			return j
		}
	}
	return nil
}

// Start launches fn as a job. It returns an error if one is already running.
func (m *Manager) Start(title string, fn func(ctx context.Context, j *Job) error) (*Job, error) {
	m.mu.Lock()
	if m.cur != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("another action is already running: %s", m.cur.Title)
	}
	m.n++
	j := &Job{ID: fmt.Sprintf("job-%d", m.n), Title: title, Status: "running", Start: time.Now()}
	m.cur = j
	m.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		err := fn(ctx, j)
		j.finish(err)
		m.mu.Lock()
		m.last = j
		m.cur = nil
		m.mu.Unlock()
	}()
	return j, nil
}

// run executes a command, streaming combined stdout/stderr into the job log,
// and returns an error if it exits non-zero. dir may be "".
func run(ctx context.Context, j *Job, dir, name string, args ...string) error {
	return runStdin(ctx, j, dir, "", name, args...)
}

// runStdin is run with optional data fed to the command's stdin (used to pipe a
// shell script to ssh, or a password to a scan).
func runStdin(ctx context.Context, j *Job, dir, stdin, name string, args ...string) error {
	j.Logf("$ %s %s", name, redact(args))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return err
	}
	pw.Close() // the child holds the only write end now
	doneScan := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			j.append(sc.Text())
		}
		pr.Close()
		close(doneScan)
	}()
	err = cmd.Wait()
	<-doneScan
	if err != nil {
		return fmt.Errorf("%s exited: %w", name, err)
	}
	return nil
}

// capture runs a command and returns its combined output, without streaming.
// Used for quick status queries.
func capture(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// streamCmd runs a prepared *exec.Cmd, streaming combined output into the job.
func streamCmd(ctx context.Context, j *Job, cmd *exec.Cmd) error {
	j.Logf("$ %s", redact(cmd.Args))
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return err
	}
	pw.Close()
	doneScan := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			j.append(sc.Text())
		}
		pr.Close()
		close(doneScan)
	}()
	err = cmd.Wait()
	<-doneScan
	if err != nil {
		return fmt.Errorf("%s exited: %w", cmd.Path, err)
	}
	return nil
}
