package labctl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/atsvetko/lab-creator/content"
)

// LabInfoSchema versions the lab-info.json contract. Consumers check it and
// refuse a major version they do not understand.
const LabInfoSchema = "lab-creator/lab-info/v1"

// LabInfo is the public description of a lab for testing tools: what
// directories exist, where they answer and which accounts to use. Passwords
// are never written here — they come from the session (UI) or the
// environment (CLI and CI).
type LabInfo struct {
	Schema      string          `json:"schema"`
	Lab         string          `json:"lab"`
	GeneratedAt time.Time       `json:"generated_at"`
	Content     string          `json:"content_version"`
	Network     InfoNetwork     `json:"network"`
	Directories []InfoDirectory `json:"directories"`
	Scanner     *InfoScanner    `json:"scanner,omitempty"`
	Secrets     string          `json:"secrets"`
}

type InfoNetwork struct {
	Switch string `json:"switch"`
	Subnet string `json:"subnet,omitempty"`
	HostIP string `json:"host_ip,omitempty"`
}

type InfoDirectory struct {
	Machine   string        `json:"machine"`
	Kind      string        `json:"kind"`
	Product   string        `json:"product"`
	Type      string        `json:"type"` // ad | samba | freeipa
	Domain    string        `json:"domain"`
	Realm     string        `json:"realm"`
	NetBIOS   string        `json:"netbios,omitempty"`
	BaseDN    string        `json:"base_dn"`
	DC        InfoHost      `json:"dc"`
	Endpoints InfoEndpoints `json:"endpoints"`
	Accounts  InfoAccounts  `json:"accounts"`
	State     InfoState     `json:"state"`
}

type InfoHost struct {
	Host string `json:"host"`
	FQDN string `json:"fqdn"`
	IP   string `json:"ip"`
}

type InfoEndpoints struct {
	LDAP     string `json:"ldap"`
	LDAPS    string `json:"ldaps,omitempty"`
	Kerberos string `json:"kerberos"`
}

type InfoAccounts struct {
	Admin string `json:"admin"` // principal (user@REALM)
	Audit string `json:"audit"` // ordinary account testers bind as
}

type InfoState struct {
	VM       string `json:"vm,omitempty"`
	Baseline bool   `json:"baseline"`
}

type InfoScanner struct {
	Machine string `json:"machine"`
	IP      string `json:"ip"`
	SSHUser string `json:"ssh_user"`
}

// BuildInfo describes the lab as configured, adding live VM state when the
// Hyper-V host is reachable.
func BuildInfo(ctx context.Context, l *Lab, cr Creds) *LabInfo {
	host, _ := hostAddr(l.Subnet)
	hostOK, _ := HostOK()
	info := &LabInfo{
		Schema: LabInfoSchema, Lab: l.Name, GeneratedAt: time.Now().UTC(),
		Content: content.Version,
		Network: InfoNetwork{Switch: l.Switch, Subnet: l.Subnet, HostIP: host},
		Secrets: "not stored here: admin and audit passwords come from the lab session or LAB_ADMIN_PASSWORD / LAB_AUDIT_PASSWORD",
	}
	for i := range l.Machines {
		m := &l.Machines[i]
		p, ok := plugin(m.Kind)
		if !ok {
			continue
		}
		if p.Scanner {
			info.Scanner = &InfoScanner{Machine: m.Name, IP: m.IP, SSHUser: m.sshUser(l)}
			continue
		}
		realm := strings.ToUpper(m.Domain)
		hostname := shortHost(m)
		admin := "Administrator@" + realm
		nb := netbios(m.Domain)
		if p.Directory == "freeipa" {
			admin = "admin@" + realm
			nb = ""
		}
		d := InfoDirectory{
			Machine: m.Name, Kind: p.Kind, Product: p.Product, Type: p.Directory,
			Domain: m.Domain, Realm: realm, NetBIOS: nb, BaseDN: baseDN(m.Domain),
			DC:        InfoHost{Host: hostname, FQDN: hostname + "." + m.Domain, IP: m.IP},
			Endpoints: InfoEndpoints{LDAP: "ldap://" + m.IP + ":389", Kerberos: m.IP + ":88"},
			Accounts:  InfoAccounts{Admin: admin, Audit: cr.auditUser() + "@" + realm},
		}
		if p.LDAPS {
			d.Endpoints.LDAPS = "ldaps://" + m.IP + ":636"
		}
		if hostOK {
			d.State.VM = VMState(ctx, m.Name)
			if d.State.VM != "" {
				d.State.Baseline = HasCheckpoint(ctx, m.Name, baselineCheckpoint)
			}
		}
		info.Directories = append(info.Directories, d)
	}
	return info
}

// InfoPath is where lab-info.json lives: in the lab's work directory.
func (l *Lab) InfoPath() string { return filepath.Join(l.Work(), "lab-info.json") }

// WriteInfo regenerates lab-info.json and returns its path.
func WriteInfo(ctx context.Context, l *Lab, cr Creds) (string, error) {
	b, err := json.MarshalIndent(BuildInfo(ctx, l, cr), "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(l.Work(), 0o755); err != nil {
		return "", err
	}
	p := l.InfoPath()
	return p, os.WriteFile(p, append(b, '\n'), 0o644)
}
