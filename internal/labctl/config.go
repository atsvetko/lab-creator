// Package labctl is the lab control panel that runs on the Hyper-V host: it
// builds dirauditor from a local clone, creates and provisions the lab VMs
// (Windows AD DCs and Samba/Альт Домен DCs) from ISOs you supply, then deploys
// the freshly built binary and scans each target. It is a separate tool from
// the read-only auditor and never linked into it.
package labctl

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Lab is the whole editable lab definition (lab.yaml). The web UI reads and
// writes it; everything the orchestrator needs comes from here.
type Lab struct {
	Name     string    `yaml:"name" json:"name"`
	Switch   string    `yaml:"switch" json:"switch"`     // Hyper-V internal switch the lab lives on
	Subnet   string    `yaml:"subnet" json:"subnet"`     // informational, e.g. 10.55.0.0/24
	RepoDir  string    `yaml:"repo_dir" json:"repo_dir"` // local clone of directory-auditor to build
	Ref      string    `yaml:"ref" json:"ref"`           // git ref to build (branch, tag or commit); default main
	WorkDir  string    `yaml:"work_dir" json:"work_dir"` // where builds, unattend files and reports land
	Machines []Machine `yaml:"machines" json:"machines"`

	path string `yaml:"-"`
}

// Machine is one lab VM.
type Machine struct {
	Name       string `yaml:"name" json:"name"`               // Hyper-V VM name
	Kind       string `yaml:"kind" json:"kind"`               // windows-dc | alt-dc
	ISO        string `yaml:"iso" json:"iso"`                 // install ISO path (you supply it)
	Domain     string `yaml:"domain" json:"domain"`           // e.g. da.test or alt.test
	IP         string `yaml:"ip" json:"ip"`                   // static IP on the lab switch
	Prefix     int    `yaml:"prefix" json:"prefix"`           // network prefix length (default 24)
	CPU        int    `yaml:"cpu" json:"cpu"`                 // vCPUs (default 2)
	MemGB      int    `yaml:"mem_gb" json:"mem_gb"`           // RAM in GB (default 4)
	DiskGB     int    `yaml:"disk_gb" json:"disk_gb"`         // system disk in GB (default 60)
	ForestMode string `yaml:"forest_mode" json:"forest_mode"` // windows-dc only
	Expect     string `yaml:"expect" json:"expect"`           // optional verify expectation file
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
	l.defaults()
	return &l, l.Validate()
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
	if l.Ref == "" {
		l.Ref = "main"
	}
	if l.Switch == "" {
		l.Switch = "da-lab"
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
	if strings.TrimSpace(l.RepoDir) == "" {
		add("repo_dir is required (the local clone of directory-auditor to build)")
	}
	seen := map[string]bool{}
	ips := map[string]bool{}
	for _, m := range l.Machines {
		if !nameRe.MatchString(m.Name) {
			add("machine name %q is invalid", m.Name)
		}
		if seen[strings.ToLower(m.Name)] {
			add("duplicate machine name %q", m.Name)
		}
		seen[strings.ToLower(m.Name)] = true
		if !kinds[m.Kind] {
			add("%s: kind must be windows-dc or alt-dc", m.Name)
		}
		if !domainRe.MatchString(m.Domain) {
			add("%s: domain %q is invalid", m.Name, m.Domain)
		}
		if !ipRe.MatchString(m.IP) {
			add("%s: ip %q is invalid", m.Name, m.IP)
		} else if ips[m.IP] {
			add("duplicate ip %s", m.IP)
		}
		ips[m.IP] = true
		if m.Prefix < 1 || m.Prefix > 32 {
			add("%s: prefix must be 1..32", m.Name)
		}
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
