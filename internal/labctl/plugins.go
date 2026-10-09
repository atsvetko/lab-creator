package labctl

import (
	"context"
	"sort"
)

// Plugin describes one kind of lab guest: an operating system / directory
// product and how to create, provision and scan it. OS support is pluggable —
// adding a new product is adding a Plugin here (plus, if needed, its lab
// scripts in the target repo's tools/lab/<name>/).
//
// Scans do not run on the guest: a separate non-domain-joined Linux scanner VM
// (a plugin with Scanner=true) runs the auditor against every target over the
// network. A plugin therefore declares what directory it is (Provider) and how
// the scanner should reach it (LDAPS), and — for a target — how to stand up the
// domain (Provision / ProvisionScript / PopulateDir).
type Plugin struct {
	Kind          string // config `kind` key, e.g. "windows-dc", "alt-domen"
	Label         string // human label for the UI
	Provider      string // auditor provider the scan uses: ad | samba | freeipa (empty for the scanner)
	OS            string // "windows" | "linux" — decides create firmware and transport
	Generation    int    // Hyper-V VM generation (2 for modern Windows/UEFI Linux)
	SecureBoot    bool   // Hyper-V Secure Boot
	DefaultExpect string // suggested verify expectation file (repo-relative)

	// Scan transport from the scanner VM. LDAPS=true pins the target's LDAPS
	// certificate and scans encrypted (Samba/FreeIPA self-sign, so this works
	// from a non-joined box); false falls back to a lab-only plaintext simple
	// bind (a vanilla Windows DC without AD CS has no LDAPS and the scanner has
	// no Kerberos, so there is no encrypted channel available).
	LDAPS bool

	// Scanner marks the dedicated auditor VM: not a scan target, not domain
	// joined, no directory of its own. It only runs the tool.
	Scanner bool

	// Target provisioning (empty for Windows, which uses a PowerShell path, and
	// for the scanner). ProvisionScript is a repo-relative vendor install+promote
	// script run over SSH; PopulateDir holds the directory-specific populate.sh
	// that seeds the deliberately-weak fixtures afterwards.
	ProvisionScript string
	PopulateDir     string

	// Provision stands up the domain and seeds fixtures. create, deploy and scan
	// are generic.
	Provision func(ctx context.Context, j *Job, l *Lab, m *Machine, cr Creds) error
}

var registry = map[string]Plugin{}

// Register adds a plugin (and is how out-of-tree builds could extend the set).
func Register(p Plugin) { registry[p.Kind] = p }

// plugin returns the plugin for a kind, resolving aliases.
func plugin(kind string) (Plugin, bool) {
	if a, ok := aliases[kind]; ok {
		kind = a
	}
	p, ok := registry[kind]
	return p, ok
}

// aliases map friendlier/legacy kind names to a registered plugin.
var aliases = map[string]string{
	"alt-dc":    "alt-domen",
	"alt-linux": "alt-domen",
	"samba":     "alt-domen",
	"freeipa":   "ald-pro",
	"scanner":   "linux-scanner",
	"auditor":   "linux-scanner",
}

// KindInfo is a registered kind as the UI lists it.
type KindInfo struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	OS      string `json:"os"`
	Scanner bool   `json:"scanner"`
}

// Kinds returns the registered plugins, sorted, for the config editor dropdown.
func Kinds() []KindInfo {
	out := make([]KindInfo, 0, len(registry))
	for _, p := range registry {
		out = append(out, KindInfo{Kind: p.Kind, Label: p.Label, OS: p.OS, Scanner: p.Scanner})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

func init() {
	Register(Plugin{
		Kind: "windows-dc", Label: "Windows Server — Active Directory DC",
		Provider: "ad", OS: "windows", Generation: 2, SecureBoot: false,
		LDAPS:         false, // vanilla DC has no LDAPS; scanner is not Kerberos-joined
		DefaultExpect: "testdata/lab/windows-expect.yaml",
		Provision:     provisionWindows,
	})
	Register(Plugin{
		Kind: "alt-domen", Label: "Альт Домен (Basealt ALT, Samba AD DC)",
		Provider: "samba", OS: "linux", Generation: 2, SecureBoot: false, LDAPS: true,
		DefaultExpect:   "testdata/lab/samba-expect.yaml",
		ProvisionScript: "tools/lab/samba/provision-vendor.sh",
		PopulateDir:     "tools/lab/samba",
		Provision:       provisionLinux,
	})
	Register(Plugin{
		Kind: "red-adm", Label: "РЕД АДМ (RED OS, Samba DC)",
		Provider: "samba", OS: "linux", Generation: 2, SecureBoot: false, LDAPS: true,
		DefaultExpect:   "testdata/lab/samba-expect.yaml",
		ProvisionScript: "tools/lab/samba/provision-vendor.sh",
		PopulateDir:     "tools/lab/samba",
		Provision:       provisionLinux,
	})
	Register(Plugin{
		Kind: "ald-pro", Label: "ALD Pro (Astra Linux, FreeIPA)",
		Provider: "freeipa", OS: "linux", Generation: 2, SecureBoot: false, LDAPS: true,
		DefaultExpect:   "testdata/lab/freeipa-admin-expect.yaml",
		ProvisionScript: "tools/lab/aldpro/provision.sh",
		PopulateDir:     "tools/lab/freeipa",
		Provision:       provisionLinux,
	})
	Register(Plugin{
		Kind: "linux-scanner", Label: "Scanner — Linux audit workstation (not domain-joined)",
		Provider: "", OS: "linux", Generation: 2, SecureBoot: false,
		Scanner:   true,
		Provision: provisionScanner,
	})
}
