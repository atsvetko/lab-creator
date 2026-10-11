// Package labctl is the lab control panel that runs on the Hyper-V host: it
// creates directory test labs on demand — VMs installed from ISOs you supply,
// promoted to domain controllers (Windows AD, Samba AD DC, FreeIPA) and seeded
// with deliberately weak objects — and describes the result in lab-info.json
// so any testing tool can use the lab. All provisioning scripts are embedded.
package labctl

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Lab is the whole editable lab definition (lab.yaml). The web UI reads and
// writes it; everything the orchestrator needs comes from here.
type Lab struct {
	Name     string    `yaml:"name" json:"name"`
	Switch   string    `yaml:"switch" json:"switch"`               // Hyper-V internal switch the lab lives on
	Subnet   string    `yaml:"subnet" json:"subnet"`               // informational, e.g. 10.55.0.0/24
	WorkDir  string    `yaml:"work_dir" json:"work_dir"`           // VHDs, answer media, extracted scripts, lab-info.json; relative = next to the lab file
	SSHUser  string    `yaml:"ssh_user,omitempty" json:"ssh_user"` // default SSH login for Linux guests; vendor DCs disable root, so this is usually a sudo-capable admin (e.g. altadmin), not root
	Machines []Machine `yaml:"machines" json:"machines"`

	// Legacy fields from the Directory Auditor era. Accepted so old lab files
	// still load, reported as warnings, and dropped on the next save.
	LegacyRepoDir string `yaml:"repo_dir,omitempty" json:"-"`
	LegacyRef     string `yaml:"ref,omitempty" json:"-"`

	path     string   `yaml:"-"`
	warnings []string `yaml:"-"`
}

// Machine is one lab VM.
type Machine struct {
	Name       string `yaml:"name" json:"name"`               // Hyper-V VM name
	Kind       string `yaml:"kind" json:"kind"`               // windows-dc | alt-domen | red-adm | ald-pro | linux-scanner
	ISO        string `yaml:"iso" json:"iso"`                 // install ISO path (you supply it)
	Domain     string `yaml:"domain" json:"domain"`           // e.g. da.test or alt.test
	IP         string `yaml:"ip" json:"ip"`                   // static IP on the lab switch
	Prefix     int    `yaml:"prefix" json:"prefix"`           // network prefix length (default 24)
	CPU        int    `yaml:"cpu" json:"cpu"`                 // vCPUs (default 2)
	MemGB      int    `yaml:"mem_gb" json:"mem_gb"`           // RAM in GB (default 4)
	DiskGB     int    `yaml:"disk_gb" json:"disk_gb"`         // system disk in GB (default 60)
	ForestMode string `yaml:"forest_mode" json:"forest_mode"` // windows-dc only
	SSHUser    string `yaml:"ssh_user" json:"ssh_user"`       // per-machine SSH login override (Linux guests); falls back to the lab default, then root

	LegacyExpect string `yaml:"expect,omitempty" json:"-"` // Directory Auditor era; ignored
}

var (
	kinds    = map[string]bool{"windows-dc": true, "alt-dc": true}
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	ipRe     = regexp.MustCompile(`^(\d{1,3}\.){3}\d{1,3}$`)
)

// Load reads a lab file, applies defaults and validates it.
func Load(path string) (*Lab, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l Lab
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	l.path = path
	l.collectLegacy()
	l.defaults()
	return &l, l.Validate()
}

// collectLegacy turns Directory Auditor-era fields into warnings and clears
// them, so the next Save writes a clean file.
func (l *Lab) collectLegacy() {
	if l.LegacyRepoDir != "" || l.LegacyRef != "" {
		l.warnings = append(l.warnings, "repo_dir/ref are no longer used (lab-creator embeds its scripts and builds no tool); they will be removed on the next save")
	}
	l.LegacyRepoDir, l.LegacyRef = "", ""
	for i := range l.Machines {
		if l.Machines[i].LegacyExpect != "" {
			l.warnings = append(l.warnings, fmt.Sprintf("%s: expect is no longer used (testing tools assert against lab-info.json); it will be removed on the next save", l.Machines[i].Name))
			l.Machines[i].LegacyExpect = ""
		}
	}
}

// Warnings returns non-fatal notes found while loading the lab file.
func (l *Lab) Warnings() []string { return l.warnings }

// Dir is the directory holding the lab file; relative paths resolve against it.
func (l *Lab) Dir() string {
	d := filepath.Dir(l.path)
	if abs, err := filepath.Abs(d); err == nil {
		return abs
	}
	return d
}

// Work returns the absolute work directory (relative work_dir is taken from
// the lab file's folder, not the current directory, so a double-clicked
// labctl and a CI job resolve the same place).
func (l *Lab) Work() string {
	if filepath.IsAbs(l.WorkDir) {
		return l.WorkDir
	}
	return filepath.Join(l.Dir(), l.WorkDir)
}

// Saved reports whether the lab file exists on disk.
func (l *Lab) Saved() bool {
	_, err := os.Stat(l.path)
	return err == nil
}

// NewDefault returns an unsaved lab with sensible defaults and no machines,
// used when no lab file exists yet so the web UI can still open and the user
// can build the lab in the config editor (the first Save writes the file).
func NewDefault(path string) *Lab {
	l := &Lab{Name: "lab", Subnet: "10.10.30.0/24", path: path}
	l.defaults()
	return l
}

// Save writes the lab file back after applying defaults; it validates first.
func (l *Lab) Save() error {
	l.defaults()
	if err := l.Validate(); err != nil {
		return err
	}
	b, err := yaml.Marshal(l)
	if err != nil {
		return err
	}
	return os.WriteFile(l.path, b, 0o644)
}

func (l *Lab) Path() string { return l.path }

func (l *Lab) defaults() {
	if l.Switch == "" {
		l.Switch = "lc-lab"
	}
	if l.WorkDir == "" {
		l.WorkDir = "labctl-work"
	}
	for i := range l.Machines {
		m := &l.Machines[i]
		if m.Prefix == 0 {
			m.Prefix = 24
		}
		if m.CPU == 0 {
			m.CPU = 2
		}
		if m.MemGB == 0 {
			m.MemGB = 4
		}
		if m.DiskGB == 0 {
			m.DiskGB = 60
		}
		if m.Kind == "windows-dc" && m.ForestMode == "" {
			m.ForestMode = "Win2016"
		}
	}
}

// Validate checks structure so a bad edit from the UI is rejected before any VM
// is touched.
func (l *Lab) Validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	// If a subnet is declared, every machine IP must fall inside it. The live
	// lab had two labs collide on 10.10.10.0/24 and one had to be re-IP'd to
	// 10.10.20.0/24; catching an out-of-subnet IP here turns that into a config
	// error before any VM is created.
	var subnet *net.IPNet
	if strings.TrimSpace(l.Subnet) != "" {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(l.Subnet)); err != nil {
			add("subnet %q is not valid CIDR (e.g. 10.10.20.0/24)", l.Subnet)
		} else {
			subnet = n
		}
	}
	hostIP, _ := hostAddr(l.Subnet)
	seen := map[string]bool{}
	ips := map[string]bool{}
	scanners := 0
	for _, m := range l.Machines {
		if m.isScanner() {
			scanners++
		}
		if !nameRe.MatchString(m.Name) {
			add("machine name %q is invalid", m.Name)
		}
		if seen[strings.ToLower(m.Name)] {
			add("duplicate machine name %q", m.Name)
		}
		seen[strings.ToLower(m.Name)] = true
		p, ok := plugin(m.Kind)
		if !ok {
			add("%s: unknown kind %q", m.Name, m.Kind)
		}
		// The scanner is not domain-joined, so it has no domain; every target does.
		if !(ok && p.Scanner) && !domainRe.MatchString(m.Domain) {
			add("%s: domain %q is invalid", m.Name, m.Domain)
		}
		if !ipRe.MatchString(m.IP) {
			add("%s: ip %q is invalid", m.Name, m.IP)
		} else if ips[m.IP] {
			add("duplicate ip %s", m.IP)
		} else if m.IP == hostIP {
			add("%s: ip %s is reserved for the host on the lab switch (first address of the subnet)", m.Name, m.IP)
		} else if subnet != nil {
			if ip := net.ParseIP(m.IP); ip == nil || !subnet.Contains(ip) {
				add("%s: ip %s is outside the lab subnet %s", m.Name, m.IP, l.Subnet)
			}
		}
		ips[m.IP] = true
		if m.Prefix < 1 || m.Prefix > 32 {
			add("%s: prefix must be 1..32", m.Name)
		}
	}
	if scanners > 1 {
		add("only one scanner machine is allowed (kind linux-scanner); all scans run from it")
	}
	if len(errs) > 0 {
		return fmt.Errorf("lab config invalid:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// Machine returns the named machine, or nil.
func (l *Lab) Machine(name string) *Machine {
	for i := range l.Machines {
		if strings.EqualFold(l.Machines[i].Name, name) {
			return &l.Machines[i]
		}
	}
	return nil
}

// Scanner returns the scanner workstation (the non-joined Linux VM testing
// tools run from), or nil if the lab has none.
func (l *Lab) Scanner() *Machine {
	for i := range l.Machines {
		if p, ok := plugin(l.Machines[i].Kind); ok && p.Scanner {
			return &l.Machines[i]
		}
	}
	return nil
}

// isScanner reports whether a machine is the scanner workstation rather than a
// directory server.
func (m *Machine) isScanner() bool {
	p, ok := plugin(m.Kind)
	return ok && p.Scanner
}

// sshUser resolves the SSH login for a Linux guest: the per-machine override,
// then the lab default, then root. Vendor DCs (Альт Домен, РЕД АДМ, ALD Pro)
// ship with root SSH login disabled — the live lab confirmed it — so in
// practice this is a sudo-capable admin account, and privileged provisioning
// steps are run through sudo (see priv() in ops.go).
func (m *Machine) sshUser(l *Lab) string {
	if m.SSHUser != "" {
		return m.SSHUser
	}
	if l != nil && l.SSHUser != "" {
		return l.SSHUser
	}
	return "root"
}
