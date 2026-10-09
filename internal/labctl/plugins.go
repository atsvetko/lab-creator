package labctl

import (
	"context"
	"sort"
)

// Plugin describes one kind of lab guest: an operating system / directory
// product and how to create, provision and scan it. OS support is pluggable —
// adding a new product is adding a Plugin here (plus, if needed, its lab
// scripts in the target repo's tools/lab/<name>/). The scan step is generic
// per OS; Provision is product-specific.
type Plugin struct {
	Kind          string // config `kind` key, e.g. "windows-dc", "alt-domen", "ald-pro"
	Label         string // human label for the UI
	Provider      string // dirauditor provider the scan uses: ad | samba | freeipa
	OS            string // "windows" | "linux" — decides create firmware and scan transport
	Generation    int    // Hyper-V VM generation (2 for modern Windows/UEFI Linux)
	SecureBoot    bool   // Hyper-V Secure Boot
	LocalScan     bool   // pass --local to the scan (Samba smb.conf checks)
	DefaultExpect string // suggested verify expectation file (repo-relative)

	// Provision promotes/joins the domain and seeds the weak fixtures. It is the
	// only product-specific action; create and scan are handled generically.
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
}

// KindInfo is a registered kind as the UI lists it.
type KindInfo struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	OS    string `json:"os"`
}

// Kinds returns the registered plugins, sorted, for the config editor dropdown.
func Kinds() []KindInfo {
	out := make([]KindInfo, 0, len(registry))
	for _, p := range registry {
		out = append(out, KindInfo{Kind: p.Kind, Label: p.Label, OS: p.OS})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

func init() {
	Register(Plugin{
		Kind: "windows-dc", Label: "Windows Server — Active Directory DC",
		Provider: "ad", OS: "windows", Generation: 2, SecureBoot: false,
		DefaultExpect: "testdata/lab/windows-expect.yaml",
		Provision:     provisionWindows,
	})
	Register(Plugin{
		Kind: "alt-domen", Label: "Альт Домен (Alt Server, Samba AD DC)",
		Provider: "samba", OS: "linux", Generation: 2, SecureBoot: false, LocalScan: true,
		DefaultExpect: "testdata/lab/samba-expect.yaml",
		Provision:     provisionSamba,
	})
	Register(Plugin{
		Kind: "ald-pro", Label: "ALD Pro (Astra, FreeIPA)",
		Provider: "freeipa", OS: "linux", Generation: 2, SecureBoot: false,
		DefaultExpect: "testdata/lab/freeipa-admin-expect.yaml",
		Provision:     provisionFreeIPA,
	})
	Register(Plugin{
		Kind: "red-adm", Label: "РЕД АДМ (RED OS, FreeIPA)",
		Provider: "freeipa", OS: "linux", Generation: 2, SecureBoot: false,
		DefaultExpect: "testdata/lab/freeipa-admin-expect.yaml",
		Provision:     provisionFreeIPA,
	})
}
