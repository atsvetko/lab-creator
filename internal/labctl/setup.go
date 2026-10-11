package labctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Check is one item of the Setup checklist. Required items gate actions; a
// warning is shown but blocks nothing.
type Check struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	OK       bool   `json:"ok"`
	Required bool   `json:"required"`
	Detail   string `json:"detail,omitempty"`
}

// hostFacts is what a PowerShell probe learns about the host. It is cached
// briefly because the UI polls and each probe spawns PowerShell.
type hostFacts struct {
	OK          bool   // Windows + PowerShell
	Why         string // reason when !OK
	HyperV      bool   // Hyper-V PowerShell module present
	Admin       bool   // elevated administrator
	HyperVAdmin bool   // member of Hyper-V Administrators (S-1-5-32-578)
	SwitchType  string // type of the lab switch, "" when it does not exist
	SSH         bool   // OpenSSH client on PATH
}

var (
	factsMu    sync.Mutex
	factsCache = map[string]hostFacts{}
	factsAt    = map[string]time.Time{}
)

// probeHost returns host facts for a lab switch name, cached for 30 seconds.
func probeHost(ctx context.Context, sw string) hostFacts {
	factsMu.Lock()
	if f, ok := factsCache[sw]; ok && time.Since(factsAt[sw]) < 30*time.Second {
		factsMu.Unlock()
		return f
	}
	factsMu.Unlock()

	var f hostFacts
	f.OK, f.Why = HostOK()
	_, err := exec.LookPath("ssh")
	f.SSH = err == nil
	if f.OK {
		script := fmt.Sprintf(`
$p = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
$admin = $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
$hv = $p.IsInRole((New-Object Security.Principal.SecurityIdentifier('S-1-5-32-578')))
$mod = [bool](Get-Module -ListAvailable -Name Hyper-V)
$sw = ''
if (Get-Command Get-VMSwitch -ErrorAction SilentlyContinue) { $s = Get-VMSwitch -Name %s -ErrorAction SilentlyContinue; if ($s) { $sw = [string]$s.SwitchType } }
"$admin|$hv|$mod|$sw"`, psQuote(sw))
		out, _ := capture(ctx, "", psExe(), "-NoProfile", "-NonInteractive", "-Command", script)
		parts := strings.Split(strings.TrimSpace(lastLine(out)), "|")
		if len(parts) == 4 {
			f.Admin = strings.EqualFold(parts[0], "True")
			f.HyperVAdmin = strings.EqualFold(parts[1], "True")
			f.HyperV = strings.EqualFold(parts[2], "True")
			f.SwitchType = parts[3]
		}
	}
	factsMu.Lock()
	factsCache[sw], factsAt[sw] = f, time.Now()
	factsMu.Unlock()
	return f
}

// forgetHost drops cached facts, e.g. after labctl created the switch.
func forgetHost() {
	factsMu.Lock()
	factsCache = map[string]hostFacts{}
	factsMu.Unlock()
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Checks builds the Setup checklist for a lab and the session credentials.
func Checks(ctx context.Context, l *Lab, cr Creds) []Check {
	f := probeHost(ctx, l.Switch)
	var cs []Check
	add := func(id, label string, ok, required bool, detail string) {
		cs = append(cs, Check{ID: id, Label: label, OK: ok, Required: required, Detail: detail})
	}

	add("host", "Windows host with PowerShell", f.OK, true, f.Why)
	add("hyperv", "Hyper-V installed", f.OK && f.HyperV, true,
		ifs(!f.HyperV && f.OK, "enable Hyper-V: Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V -All, then reboot"))
	add("rights", "Rights to manage Hyper-V", f.Admin || f.HyperVAdmin, true,
		ifs(!(f.Admin || f.HyperVAdmin), "run labctl as administrator, or join the local group Hyper-V Administrators"))

	switch {
	case f.SwitchType == "":
		add("switch", fmt.Sprintf("Lab switch %q", l.Switch), f.Admin, false,
			ifs(f.Admin, "will be created as an internal switch on the first create",
				"does not exist; the first create needs labctl run as administrator to create it"))
	case f.SwitchType == "Internal" || f.SwitchType == "Private":
		add("switch", fmt.Sprintf("Lab switch %q is isolated (%s)", l.Switch, f.SwitchType), true, true, "")
	default:
		add("switch", fmt.Sprintf("Lab switch %q is isolated", l.Switch), false, true,
			fmt.Sprintf("it is %s — a weak lab must not reach a real network; pick another switch name", f.SwitchType))
	}

	add("saved", "Lab file saved", l.Saved(), true,
		ifs(!l.Saved(), "open Config, add machines and Save — this writes "+l.Path()))
	verr := l.Validate()
	add("valid", "Lab configuration valid", verr == nil, true, errText(verr))
	targets := 0
	linux := false
	for _, m := range l.Machines {
		if !m.isScanner() {
			targets++
		}
		if p, ok := plugin(m.Kind); ok && p.OS == "linux" {
			linux = true
		}
	}
	add("targets", "At least one directory server", targets > 0, true, ifs(targets == 0, "add a machine of kind windows-dc, alt-domen, red-adm or ald-pro"))

	var missing []string
	for _, m := range l.Machines {
		if m.ISO == "" {
			missing = append(missing, m.Name+": no ISO set")
		} else if _, err := os.Stat(m.ISO); err != nil {
			missing = append(missing, m.Name+": "+m.ISO+" not found")
		}
	}
	add("isos", "Install ISOs present", len(missing) == 0 && len(l.Machines) > 0, false, strings.Join(missing, "; "))

	add("creds", "Lab credentials set", cr.AdminPassword != "" && cr.AuditPassword != "", true,
		ifs(!(cr.AdminPassword != "" && cr.AuditPassword != ""), "set the admin and audit passwords (held in memory only)"))
	if linux {
		add("ssh", "OpenSSH client (for Linux guests)", f.SSH, true,
			ifs(!f.SSH, "install it: Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0"))
	}
	return cs
}

// Ready reports whether every required check passed.
func Ready(cs []Check) bool {
	for _, c := range cs {
		if c.Required && !c.OK {
			return false
		}
	}
	return true
}

// gate returns an error naming the first unmet requirement for an action on a
// machine (m may be nil for lab-wide actions). Power and checkpoint actions
// only need the host; create and provision need the full checklist.
func gate(cs []Check, action string, m *Machine) error {
	need := map[string]bool{"host": true, "hyperv": true, "rights": true}
	switch action {
	case "create":
		for _, id := range []string{"switch", "saved", "valid", "creds"} {
			need[id] = true
		}
	case "provision", "up":
		for _, id := range []string{"switch", "saved", "valid", "creds", "ssh"} {
			need[id] = true
		}
	}
	for _, c := range cs {
		if need[c.ID] && !c.OK && (c.Required || c.ID == "switch") {
			msg := c.Label + ": not ready"
			if c.Detail != "" {
				msg += " — " + c.Detail
			}
			return fmt.Errorf("%s", msg)
		}
	}
	if action == "create" && m != nil {
		if m.ISO == "" {
			return fmt.Errorf("%s: no install ISO set (Config)", m.Name)
		}
		if _, err := os.Stat(m.ISO); err != nil {
			return fmt.Errorf("%s: install ISO not found: %s", m.Name, m.ISO)
		}
	}
	return nil
}

func ifs(cond bool, yes string, no ...string) string {
	if cond {
		return yes
	}
	if len(no) > 0 {
		return no[0]
	}
	return ""
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
