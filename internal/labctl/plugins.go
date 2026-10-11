package labctl

import (
	"context"
	"sort"
)

// Plugin describes one kind of lab guest: an operating system / directory
// product and how to create and provision it. Adding a product is adding a
// Plugin here plus, if needed, its scripts under content/.
//
// A plugin declares what directory it serves (Directory) and how a client can
// reach it (LDAPS) so lab-info.json can describe the lab to testing tools.
type Plugin struct {
	Kind       string // config `kind` key, e.g. "windows-dc", "alt-domen"
	Label      string // human label for the UI
	Product    string // product name for lab-info.json
	Directory  string // ad | samba | freeipa (empty for the scanner)
	OS         string // "windows" | "linux" — decides create firmware and transport
	Generation int    // Hyper-V VM generation (2 for modern Windows/UEFI Linux)
	SecureBoot bool   // Hyper-V Secure Boot

	// LDAPS reports whether the provisioned directory serves LDAPS out of the
	// box (Samba and FreeIPA self-sign; a vanilla Windows DC without AD CS does not).
	LDAPS bool

	// Scanner marks a non-joined Linux workstation from which testing tools
	// run against the directories. It has no directory of its own.
	Scanner bool

	// Linux targets: content-relative scripts run over SSH. ProvisionScript
	// installs the vendor's directory packages and promotes the DC;
	// PopulateScript seeds the deliberately weak objects afterwards.
	ProvisionScript string
	PopulateScript  string

	// Provision stands up the domain and seeds fixtures. create is generic.
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
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	OS        string `json:"os"`
	Directory string `json:"directory"`
	Scanner   bool   `json:"scanner"`
}

// Kinds returns the registered plugins, sorted, for the config editor dropdown.
func Kinds() []KindInfo {
	out := make([]KindInfo, 0, len(registry))
	for _, p := range registry {
		out = append(out, KindInfo{Kind: p.Kind, Label: p.Label, OS: p.OS, Directory: p.Directory, Scanner: p.Scanner})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

func init() {
	Register(Plugin{
		Kind: "windows-dc", Label: "Windows Server — Active Directory DC",
		Product: "Windows Server", Directory: "ad", OS: "windows", Generation: 2,
		Provision: provisionWindows,
	})
	Register(Plugin{
		Kind: "alt-domen", Label: "Альт Домен (Basealt ALT, Samba AD DC)",
		Product: "Альт Домен", Directory: "samba", OS: "linux", Generation: 2, LDAPS: true,
		ProvisionScript: "samba/provision-vendor.sh",
		PopulateScript:  "samba/populate.sh",
		Provision:       provisionLinux,
	})
	Register(Plugin{
		Kind: "red-adm", Label: "РЕД АДМ (RED OS, Samba DC)",
		Product: "РЕД АДМ", Directory: "samba", OS: "linux", Generation: 2, LDAPS: true,
		ProvisionScript: "samba/provision-vendor.sh",
		PopulateScript:  "samba/populate.sh",
		Provision:       provisionLinux,
	})
	Register(Plugin{
		Kind: "ald-pro", Label: "ALD Pro (Astra Linux, FreeIPA)",
		Product: "ALD Pro", Directory: "freeipa", OS: "linux", Generation: 2, LDAPS: true,
		ProvisionScript: "aldpro/provision.sh",
		PopulateScript:  "freeipa/populate.sh",
		Provision:       provisionLinux,
	})
	Register(Plugin{
		Kind: "linux-scanner", Label: "Scanner — Linux workstation for testing tools (not domain-joined)",
		Product: "Linux", OS: "linux", Generation: 2,
		Scanner:   true,
		Provision: provisionScanner,
	})
}
