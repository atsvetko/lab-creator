package labctl

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/atsvetko/lab-creator/content"
)

// baselineCheckpoint is the Hyper-V checkpoint taken after provisioning;
// reset restores it. The name predates lab-creator and is kept so existing
// labs keep their baselines.
const baselineCheckpoint = "vuln-baseline"

// Creds are the throw-away lab credentials, held in memory for the session only
// and never written to the lab file. Provisioning sets the admin password on
// every DC and creates the audit account testing tools bind as.
type Creds struct {
	AdminPassword string // domain Administrator / IPA admin after promotion
	AuditPassword string // the ordinary audit account
	AuditUser     string // default "audit"
}

func (c Creds) auditUser() string {
	if c.AuditUser != "" {
		return c.AuditUser
	}
	return "audit"
}

// userPassword is the password given to the other seeded (weak) accounts. It
// is a fixture, not a secret: the weaknesses under test are elsewhere.
const userPassword = "Lab-User-Pass-2026!"

// Vars is the LAB_* environment every provisioning script receives. The names
// are the contract between labctl and content/: each value has one meaning
// (DNS domain, NetBIOS name, realm and base DN are separate variables).
func Vars(m *Machine, cr Creds) map[string]string {
	return map[string]string{
		"LABCTL_LAB":         "1",
		"LAB_DNS_DOMAIN":     m.Domain,
		"LAB_REALM":          strings.ToUpper(m.Domain),
		"LAB_NETBIOS":        netbios(m.Domain),
		"LAB_BASEDN":         baseDN(m.Domain),
		"LAB_HOST":           shortHost(m),
		"LAB_IP":             m.IP,
		"LAB_PREFIX":         fmt.Sprint(m.Prefix),
		"LAB_ADMIN_PASSWORD": cr.AdminPassword,
		"LAB_AUDIT_USER":     cr.auditUser(),
		"LAB_AUDIT_PASSWORD": cr.AuditPassword,
		"LAB_USER_PASSWORD":  userPassword,
	}
}

// baseDN turns a DNS domain into an LDAP base DN (corp.test → DC=corp,DC=test).
func baseDN(domain string) string {
	parts := strings.Split(strings.Trim(domain, "."), ".")
	for i, p := range parts {
		parts[i] = "DC=" + p
	}
	return strings.Join(parts, ",")
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
			time.Sleep(8 * time.Second) // let the directory finish starting after the port opens
			j.Logf("%s is answering LDAP", ip)
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("%s did not answer LDAP within %s", ip, timeout)
}

// provisionWindows promotes the VM to a DC and seeds the weak fixtures, over
// PowerShell Direct, using the embedded content/windows scripts.
func provisionWindows(ctx context.Context, j *Job, l *Lab, m *Machine, cr Creds) error {
	if ok, why := HostOK(); !ok {
		return fmt.Errorf("provision needs the Windows host: %s", why)
	}
	root, err := content.Extract(filepath.Join(l.Work(), "content"))
	if err != nil {
		return fmt.Errorf("extract provisioning scripts: %w", err)
	}
	provPath := filepath.Join(root, "windows", "provision.ps1")
	popPath := filepath.Join(root, "windows", "populate.ps1")
	j.Logf("%s: provisioning scripts %s (content %s)", m.Name, root, content.Version)

	// One orchestration script: build the credential from an env var, run
	// provision (reboots), wait, then populate. Secrets reach PowerShell
	// through the environment, never the command line.
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$pw  = ConvertTo-SecureString $env:LAB_ADMIN_PASSWORD -AsPlainText -Force
$cred = New-Object System.Management.Automation.PSCredential('Administrator', $pw)
$dacred = New-Object System.Management.Automation.PSCredential(%s, $pw)
$prov = Get-Content -Raw %s
$pop  = Get-Content -Raw %s
Write-Host 'promoting (will reboot the guest)...'
try {
  Invoke-Command -VMName %s -Credential $cred -ScriptBlock {
    param($code,$dom,$nb,$ip,$prefix,$mode)
    $env:LABCTL_LAB='1'
    & ([scriptblock]::Create($code)) -DomainName $dom -NetBiosName $nb -IPAddress $ip -PrefixLength $prefix -ForestMode $mode -Confirm:$false
  } -ArgumentList $prov, %s, %s, %s, %d, %s
} catch { Write-Host "provision step returned (guest may be rebooting): $_" }
Start-Sleep -Seconds 60
Write-Host 'seeding fixtures...'
Invoke-Command -VMName %s -Credential $dacred -ScriptBlock {
  param($code,$dom,$auser,$apw)
  $env:LABCTL_LAB='1'
  & ([scriptblock]::Create($code)) -DomainName $dom -AuditUser $auser -AuditPassword $apw
} -ArgumentList $pop, %s, $env:LAB_AUDIT_USER, $env:LAB_AUDIT_PASSWORD
Write-Host 'windows provisioning done'
`,
		psQuote(netbios(m.Domain)+`\Administrator`),
		psQuote(provPath), psQuote(popPath),
		psQuote(m.Name), psQuote(m.Domain), psQuote(netbios(m.Domain)), psQuote(m.IP), m.Prefix, psQuote(m.ForestMode),
		psQuote(m.Name), psQuote(m.Domain))
	v := Vars(m, cr)
	return psEnv(ctx, j, script, map[string]string{
		"LAB_ADMIN_PASSWORD": v["LAB_ADMIN_PASSWORD"],
		"LAB_AUDIT_USER":     v["LAB_AUDIT_USER"],
		"LAB_AUDIT_PASSWORD": v["LAB_AUDIT_PASSWORD"],
	})
}

// provisionLinux stands up a Linux target DC over SSH: the plugin's vendor
// install+promote script (vendor packages, never upstream builds), then the
// directory-specific populate script that seeds the weak fixtures. Both come
// from the embedded content; parameters and secrets travel inside the piped
// script, so they never appear on a command line on either machine.
func provisionLinux(ctx context.Context, j *Job, l *Lab, m *Machine, cr Creds) error {
	p, _ := plugin(m.Kind)
	user := m.sshUser(l)
	remote := priv(user, "bash -s")
	for _, s := range []struct{ name, path string }{
		{"provision", p.ProvisionScript},
		{"populate", p.PopulateScript},
	} {
		if s.path == "" {
			continue
		}
		body, err := content.Read(s.path)
		if err != nil {
			return fmt.Errorf("embedded script %s: %w", s.path, err)
		}
		j.Logf("%s: %s (%s) over ssh to %s@%s", m.Name, s.name, s.path, user, m.IP)
		if err := sshScript(ctx, j, user, m.IP, remote, exportPreamble(Vars(m, cr))+string(body)); err != nil {
			return err
		}
	}
	return nil
}

// exportPreamble turns vars into `export K='v'` lines prepended to a piped
// script, so values reach the script without touching argv or sudo's env reset.
func exportPreamble(vars map[string]string) string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%s\n", k, shQuote(vars[k]))
	}
	return b.String()
}

// provisionScanner prepares the scanner workstation: it is not domain-joined
// and has no directory of its own, so there is nothing to promote — we only
// confirm we can reach it over SSH.
func provisionScanner(ctx context.Context, j *Job, l *Lab, m *Machine, cr Creds) error {
	user := m.sshUser(l)
	j.Logf("scanner %s: not domain-joined — no provisioning; checking SSH to %s@%s", m.Name, user, m.IP)
	return sshScript(ctx, j, user, m.IP, "bash -s", "echo scanner reachable: $(uname -a)")
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
// install/promote scripts still need root, which sudo supplies.
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
		h = strings.ToLower(netbios(m.Domain))
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
