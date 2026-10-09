package labctl

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Creds are the throw-away lab credentials, held in memory for the session only
// and never written to the lab file. Provisioning uses the admin password;
// scans authenticate as the ordinary audit user.
type Creds struct {
	AdminPassword string // DA\Administrator (Windows) / root (Alt) after promotion
	AuditPassword string // the ordinary audit account the scan uses
	AuditUser     string // default "audit"
}

func (c Creds) auditUser() string {
	if c.AuditUser != "" {
		return c.AuditUser
	}
	return "audit"
}

// waitLDAP blocks until the DC answers on 389, or the timeout elapses.
func waitLDAP(ctx context.Context, j *Job, ip string, timeout time.Duration) error {
	j.Logf("waiting for LDAP on %s:389 (up to %s)", ip, timeout)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "389"), 3*time.Second)
		if err == nil {
			c.Close()
			time.Sleep(8 * time.Second) // let AD DS finish starting after the port opens
			j.Logf("%s is answering LDAP", ip)
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("%s did not answer LDAP within %s", ip, timeout)
}

// provisionWindows promotes the VM to a DC and seeds the weak fixtures, over
// PowerShell Direct, using the host's tools/lab/windows scripts.
func provisionWindows(ctx context.Context, j *Job, l *Lab, m *Machine, cr Creds) error {
	ok, why := HostOK()
	if !ok {
		return fmt.Errorf("provision needs the Windows host: %s", why)
	}
	provPath := filepath.Join(l.RepoDir, "tools", "lab", "windows", "provision.ps1")
	popPath := filepath.Join(l.RepoDir, "tools", "lab", "windows", "populate.ps1")
	for _, p := range []string{provPath, popPath} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("missing %s (is repo_dir correct?)", p)
		}
	}
	// One orchestration script: build the credential from an env var, run
	// provision (reboots), wait for LDAP, then populate. The password reaches
	// PowerShell through the environment, not the command line.
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$pw  = ConvertTo-SecureString $env:LAB_ADMIN_PASSWORD -AsPlainText -Force
$cred = New-Object System.Management.Automation.PSCredential('Administrator', $pw)
$dacred = New-Object System.Management.Automation.PSCredential('%s\Administrator', $pw)
$prov = Get-Content -Raw %s
$pop  = Get-Content -Raw %s
Write-Host 'promoting (will reboot the guest)...'
try {
  Invoke-Command -VMName %s -Credential $cred -ScriptBlock {
    param($code,$dom,$ip)
    $env:DIRAUDITOR_LAB='1'; & ([scriptblock]::Create($code)) -DomainName $dom -IPAddress $ip
  } -ArgumentList $prov, %s, %s
} catch { Write-Host "provision step returned (guest may be rebooting): $_" }
Start-Sleep -Seconds 60
Write-Host 'seeding fixtures...'
Invoke-Command -VMName %s -Credential $dacred -ScriptBlock {
  param($code,$dom)
  $env:DIRAUDITOR_LAB='1'; & ([scriptblock]::Create($code)) -DomainName $dom
} -ArgumentList $pop, %s
Write-Host 'windows provisioning done'
`,
		netbios(m.Domain),
		psQuote(provPath), psQuote(popPath),
		psQuote(m.Name), psQuote(m.Domain), psQuote(m.IP),
		psQuote(m.Name), psQuote(m.Domain))
	return psEnv(ctx, j, script, map[string]string{"LAB_ADMIN_PASSWORD": cr.AdminPassword})
}

// provisionAlt provisions a Samba-based Альт Домен DC over SSH using the
// tools/lab/samba scripts. Assumes key-based SSH to root@<ip> is set up.
func provisionAlt(ctx context.Context, j *Job, l *Lab, m *Machine, cr Creds) error {
	prov := filepath.Join(l.RepoDir, "tools", "lab", "samba", "provision.sh")
	pop := filepath.Join(l.RepoDir, "tools", "lab", "samba", "populate.sh")
	for _, p := range []string{prov, pop} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("missing %s", p)
		}
	}
	realm := strings.ToUpper(m.Domain)
	for _, step := range []struct{ name, path string }{{"provision", prov}, {"populate", pop}} {
		b, err := os.ReadFile(step.path)
		if err != nil {
			return err
		}
		j.Logf("alt %s over ssh to %s", step.name, m.IP)
		env := fmt.Sprintf("DIRAUDITOR_LAB=1 LAB_REALM=%s", shQuote(realm))
		if err := sshScript(ctx, j, m.IP, env+" bash -s", string(b)); err != nil {
			return err
		}
	}
	return nil
}

// deployAndScan runs the freshly built auditor against the machine as the
// ordinary audit user and writes a report under WorkDir/reports/<name>.
func deployAndScan(ctx context.Context, j *Job, l *Lab, b *Built, m *Machine, cr Creds) (string, error) {
	outDir := filepath.Join(l.WorkDir, "reports", m.Name)
	_ = os.MkdirAll(outDir, 0o755)
	cat := filepath.Join(l.RepoDir, "catalogue")
	user := fmt.Sprintf("%s@%s", cr.auditUser(), m.Domain)

	if m.Kind == "alt-dc" {
		// Full coverage (incl. smb.conf checks) needs the Linux binary on the box.
		remote := "/tmp/dirauditor"
		if err := scp(ctx, j, b.LinBin, fmt.Sprintf("root@%s:%s", m.IP, remote)); err != nil {
			return "", err
		}
		cmd := fmt.Sprintf("DIRAUDITOR_LAB=1 %s scan --server %s --domain %s --user %s --password-stdin --local --catalogue /tmp/catalogue --packs '' --out /tmp/lab-out --lang en; cat /tmp/lab-out/report.json",
			remote, m.IP, m.Domain, user)
		// copy the catalogue too
		if err := scp(ctx, j, cat, fmt.Sprintf("root@%s:/tmp/catalogue", m.IP)); err != nil {
			return "", err
		}
		rep := filepath.Join(outDir, "report.json")
		if err := sshCaptureToFile(ctx, j, m.IP, cmd, cr.AuditPassword, rep); err != nil {
			return "", err
		}
		return rep, nil
	}

	// Windows DC (or any AD DC): scan over the network from the host.
	exe := b.WinExe
	if _, err := os.Stat(exe); err != nil {
		return "", fmt.Errorf("built binary missing: %s (run Build first)", exe)
	}
	cmd := exec.CommandContext(ctx, exe, "scan",
		"--server", m.IP, "--domain", m.Domain, "--user", user, "--password-stdin",
		"--catalogue", cat, "--packs", "", "--out", outDir, "--lang", "en")
	cmd.Stdin = strings.NewReader(cr.AuditPassword)
	if err := streamCmd(ctx, j, cmd); err != nil {
		return "", err
	}
	return filepath.Join(outDir, "report.json"), nil
}

// verify runs the repo's verify tool against a report and the machine's expect
// file, returning whether it passed.
func verify(ctx context.Context, j *Job, l *Lab, m *Machine, report string) (bool, error) {
	if m.Expect == "" {
		j.Logf("no expect file for %s — skipping verify", m.Name)
		return true, nil
	}
	expect := filepath.Join(l.RepoDir, m.Expect)
	cmd := exec.CommandContext(ctx, goExe(), "run", "./tools/lab/verify", "-report", report, "-expect", expect)
	cmd.Dir = l.RepoDir
	err := streamCmd(ctx, j, cmd)
	return err == nil, err
}

// --- small shell/ssh helpers ------------------------------------------------

func psEnv(ctx context.Context, j *Job, script string, env map[string]string) error {
	exe := psExe()
	if exe == "" {
		return fmt.Errorf("PowerShell not available (run labctl on the Hyper-V host)")
	}
	cmd := exec.CommandContext(ctx, exe, "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return streamCmd(ctx, j, cmd)
}

func sshScript(ctx context.Context, j *Job, ip, remoteCmd, stdin string) error {
	args := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new",
		"root@" + ip, remoteCmd}
	return runStdin(ctx, j, "", stdin, "ssh", args...)
}

func scp(ctx context.Context, j *Job, src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	args := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new"}
	if st.IsDir() {
		args = append(args, "-r")
	}
	args = append(args, src, dst)
	return run(ctx, j, "", "scp", args...)
}

// sshCaptureToFile runs a remote command (password on stdin) and saves its
// stdout (the report JSON printed last) to a local file.
func sshCaptureToFile(ctx context.Context, j *Job, ip, remoteCmd, stdin, outFile string) error {
	exe, err := exec.LookPath("ssh")
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, exe, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "root@"+ip, remoteCmd)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		j.append(string(out))
		return err
	}
	// The report JSON is the tail after our other output; find the last '{'...'}' block.
	s := string(out)
	if i := strings.LastIndex(s, "\n{"); i >= 0 {
		s = s[i+1:]
	}
	return os.WriteFile(outFile, []byte(s), 0o644)
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// netbios derives a NetBIOS-ish name from a DNS domain (first label, upper).
func netbios(domain string) string {
	d := domain
	if i := strings.IndexByte(d, '.'); i >= 0 {
		d = d[:i]
	}
	if len(d) > 15 {
		d = d[:15]
	}
	return strings.ToUpper(d)
}
