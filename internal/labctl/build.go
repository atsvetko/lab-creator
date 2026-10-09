package labctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Built records what the last successful build produced.
type Built struct {
	Ref     string `json:"ref"`
	Commit  string `json:"commit"`
	Version string `json:"version"`
	WinExe  string `json:"win_exe"`
	LinBin  string `json:"lin_bin"`
}

// Build checks out the requested ref in the local clone and compiles dirauditor
// for Windows and Linux into the work directory. "latest available" = whatever
// the clone's remote has for that ref, fetched first.
func Build(ctx context.Context, j *Job, l *Lab, ref string) (*Built, error) {
	if ref == "" {
		ref = l.Ref
	}
	repo := l.RepoDir
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		return nil, fmt.Errorf("repo_dir %q is not a Go module (no go.mod): %w", repo, err)
	}
	j.Logf("building ref %q from %s", ref, repo)

	// Fetch and check out. A branch is fast-forwarded; a tag/commit is detached.
	if err := run(ctx, j, repo, "git", "fetch", "--all", "--tags", "--prune"); err != nil {
		return nil, err
	}
	if err := run(ctx, j, repo, "git", "checkout", "--force", ref); err != nil {
		return nil, err
	}
	// Fast-forward if ref is a branch (ignore failure for detached tags/commits).
	_ = run(ctx, j, repo, "git", "merge", "--ff-only", "@{u}")

	commit, _ := capture(ctx, repo, "git", "rev-parse", "--short=12", "HEAD")
	version, _ := capture(ctx, repo, "git", "describe", "--tags", "--always", "--dirty")

	bin := filepath.Join(l.WorkDir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return nil, err
	}
	ld := fmt.Sprintf("-buildid= -X github.com/atsvetko/directory-auditor/internal/buildinfo.Version=%s -X github.com/atsvetko/directory-auditor/internal/buildinfo.Commit=%s", version, commit)

	winExe := filepath.Join(bin, "dirauditor.exe")
	if err := goBuild(ctx, j, repo, "windows", "amd64", ld, winExe); err != nil {
		return nil, err
	}
	linBin := filepath.Join(bin, "dirauditor")
	if err := goBuild(ctx, j, repo, "linux", "amd64", ld, linBin); err != nil {
		return nil, err
	}
	b := &Built{Ref: ref, Commit: commit, Version: version, WinExe: winExe, LinBin: linBin}
	j.Logf("built %s (%s) → %s + %s", version, commit, winExe, linBin)
	return b, nil
}

func goBuild(ctx context.Context, j *Job, repo, goos, goarch, ld, out string) error {
	j.Logf("go build %s/%s → %s", goos, goarch, filepath.Base(out))
	// A per-call environment: CGO off, static, trimmed. Reuse the host toolchain.
	cmd := exec.CommandContext(ctx, goExe(), "build", "-trimpath", "-ldflags", ld, "-o", out, "./cmd/dirauditor")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	return streamCmd(ctx, j, cmd)
}

func goExe() string {
	if runtime.GOOS == "windows" {
		return "go.exe"
	}
	return "go"
}
