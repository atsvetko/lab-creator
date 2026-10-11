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

// provisionLinux stands up a Linux target DC over SSH: it runs the plugin's
// vendor install+promote script (which installs the vendor's own Samba/FreeIPA
// packages — never upstream) and then the directory-specific populate.sh that
// seeds the deliberately-weak fixtures. Key-based SSH to root@<ip> is assumed.
func provisionLinux(ctx context.Context, j *Job, l *Lab, m *Machine, cr Creds) error {
	p, _ := plugin(m.Kind)
	user := m.sshUser(l)
	vars := fmt.Sprintf("DIRAUDITOR_LAB=1 LAB_REALM=%s LAB_DOMAIN=%s LAB_HOST=%s LAB_IP=%s LAB_ADMIN_PASSWORD=%s",
		shQuote(strings.ToUpper(m.Domain)), shQuote(m.Domain), shQuote(shortHost(m)), shQuote(m.IP), shQuote(cr.AdminPassword))
	// `env VAR=… bash -s`, wrapped in sudo when not logging in as root, so the
	// variables survive the privilege change (sudo resets the environment).
	remote := priv(user, "env "+vars+" bash -s")

	type step struct {
		name, path string
		optional   bool
	}
	var steps []step
	if p.ProvisionScript != "" {
		steps = append(steps, step{"provision", filepath.Join(l.RepoDir, filepath.FromSlash(p.ProvisionScript)), false})
	}
	if p.PopulateDir != "" {
		steps = append(steps, step{"populate", filepath.Join(l.RepoDir, filepath.FromSlash(p.PopulateDir), "populate.sh"), false})
	}
	for _, s := range steps {
		b, err := os.ReadFile(s.path)
		if err != nil {
			if s.optional {
				continue
			}
			return fmt.Errorf("missing %s (is repo_dir correct?)", s.path)
		}
		j.Logf("%s: %s over ssh to %s@%s", m.Kind, s.name, user, m.IP)
		if err := sshScript(ctx, j, user, m.IP, remote, string(b)); err != nil {
			return err
		}
	}
	return nil
}

// provisionScanner prepares the dedicated scanner VM: it is not domain-joined
// and has no directory of its own, so there is nothing to promote — we only
// confirm we can reach it over SSH (the tool is copied there at scan time).
func provisionScanner(ctx context.Context, j *Job, l *Lab, m *Machine, cr Creds) error {
	user := m.sshUser(l)
	j.Logf("scanner %s: not domain-joined — no provisioning; checking SSH to %s@%s", m.Name, user, m.IP)
	return sshScript(ctx, j, user, m.IP, "bash -s", "echo scanner reachable: $(uname -a)")
}

// deployAndScan runs every scan FROM the dedicated scanner VM (a non-joined
// Linux box) against the target DC over the network, as the ordinary audit
// user, and writes a report under WorkDir/reports/<name>. The scanner never
// joins any domain; it binds with explicit credentials and the right provider.
func deployAndScan(ctx context.Context, j *Job, l *Lab, b *Built, m *Machine, cr Creds) (string, error) {
	sc := l.Scanner()
	if sc == nil {
		return "", fmt.Errorf("no scanner machine in the lab — add one (kind: linux-scanner); all scans run from it")
	}
	if m.isScanner() {
		return "", fmt.Errorf("%s is the scanner, not a scan target", m.Name)
	}
	outDir := filepath.Join(l.WorkDir, "reports", m.Name)
	_ = os.MkdirAll(outDir, 0o755)

	// Put the freshly built Linux binary and the catalogue on the scanner, in
	// the login user's home — scanning is read-only and needs no privilege, so
	// we avoid /opt (which would require root the vendor account may not have).
	scUser := sc.sshUser(l)
	if _, err := os.Stat(b.LinBin); err != nil {
		return "", fmt.Errorf("built Linux binary missing: %s (run Build first)", b.LinBin)
	}
	if err := scp(ctx, j, b.LinBin, fmt.Sprintf("%s:dirauditor", sshTarget(scUser, sc.IP))); err != nil {
		return "", err
	}
	if err := scp(ctx, j, filepath.Join(l.RepoDir, "catalogue"), fmt.Sprintf("%s:dirauditor-catalogue", sshTarget(scUser, sc.IP))); err != nil {
		return "", err
	}
	if err := sshScript(ctx, j, scUser, sc.IP, "bash -s", "chmod +x ./dirauditor"); err != nil {
		return "", err
	}

	p, _ := plugin(m.Kind)
	user := fmt.Sprintf("%s@%s", cr.auditUser(), m.Domain)

	// Transport. Samba/FreeIPA self-sign, so from the non-joined scanner we pin
	// the target's LDAPS certificate and stay encrypted. A vanilla Windows DC
	// has no LDAPS and the scanner has no Kerberos, so fall back to a lab-only
	// plaintext simple bind on the isolated switch.
	tls := "--tls none --insecure-plaintext"
	if p.LDAPS {
		pin, err := ldapsPin(ctx, j, scUser, sc.IP, m.IP)
		if err != nil || pin == "" {
			j.Logf("%s: could not fetch LDAPS pin from %s (%v) — the scan will fail closed rather than drop encryption; check the DC is up and serving 636", m.Name, m.IP, err)
			return "", fmt.Errorf("no LDAPS certificate from %s:636", m.IP)
		}
		tls = fmt.Sprintf("--tls ldaps --pin %s", pin)
		j.Logf("%s: pinned LDAPS cert %s…", m.Name, pin[:min(16, len(pin))])
	} else {
		j.Logf("%s: no LDAPS/Kerberos available from a non-joined scanner — using a lab-only plaintext bind on the isolated switch", m.Name)
	}

	remote := fmt.Sprintf(
		"DIRAUDITOR_LAB=1 ./dirauditor scan --provider %s --server %s --domain %s --user %s --password-stdin %s --catalogue ./dirauditor-catalogue --packs '' --out ./lab-out --lang en >&2; cat ./lab-out/report.json",
		shQuote(p.Provider), shQuote(m.IP), shQuote(m.Domain), shQuote(user), tls)
	rep := filepath.Join(outDir, "report.json")
	j.Logf("scan %s (%s) from scanner %s@%s", m.Name, p.Provider, scUser, sc.Name)
	if err := sshCaptureToFile(ctx, j, scUser, sc.IP, remote, cr.AuditPassword, rep); err != nil {
		return "", err
	}
	return rep, nil
}

// ldapsPin fetches the SHA-256 fingerprint of a target's LDAPS certificate,
// from the scanner (so it matches what the scan will actually connect to).
func ldapsPin(ctx context.Context, j *Job, scannerUser, scannerIP, targetIP string) (string, error) {
	cmd := fmt.Sprintf("echo | openssl s_client -connect %s:636 -servername %s 2>/dev/null | openssl x509 -noout -fingerprint -sha256 | cut -d= -f2 | tr -d ':' | tr 'A-F' 'a-f'",
		targetIP, targetIP)
	out, err := sshOut(ctx, scannerUser, scannerIP, cmd)
	return strings.TrimSpace(out), err
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

// priv wraps a remote command in sudo when the SSH login is not root. Vendor
// DCs disable root SSH, so provisioning connects as a sudo-capable admin; the
// install/promote scripts still need root, which sudo supplies. Read-only scans
// never go through priv — the auditor needs no privilege to bind over LDAP.
func priv(user, cmd string) string {
	if user == "" || user == "root" {
		return cmd
	}
	return "sudo " + cmd
}

func sshScript(ctx context.Context, j *Job, user, ip, remoteCmd, stdin string) error {
	args := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new",
		sshTarget(user, ip), remoteCmd}
	return runStdin(ctx, j, "", stdin, "ssh", args...)
}

// sshTarget builds user@ip, defaulting to root when no user is given.
func sshTarget(user, ip string) string {
	if user == "" {
		user = "root"
	}
	return user + "@" + ip
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
func sshCaptureToFile(ctx context.Context, j *Job, user, ip, remoteCmd, stdin, outFile string) error {
	exe, err := exec.LookPath("ssh")
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, exe, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", sshTarget(user, ip), remoteCmd)
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

// sshOut runs a remote command and returns its stdout (no stdin, not streamed).
func sshOut(ctx context.Context, user, ip, remoteCmd string) (string, error) {
	exe, err := exec.LookPath("ssh")
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, exe, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", sshTarget(user, ip), remoteCmd)
	out, err := cmd.Output()
	return string(out), err
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// shortHost derives a lab DC hostname from the machine name (lowercase, only
// letters, digits and hyphens), falling back to the first domain label.
func shortHost(m *Machine) string {
	var b strings.Builder
	for _, r := range strings.ToLower(m.Name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		}
	}
	h := strings.Trim(b.String(), "-")
	if h == "" {
		h = netbios(m.Domain)
		h = strings.ToLower(h)
	}
	if len(h) > 15 {
		h = h[:15]
	}
	if h == "" {
		h = "dc1"
	}
	return h
}

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
