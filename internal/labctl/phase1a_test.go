package labctl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atsvetko/lab-creator/content"
)

// A Directory Auditor-era lab file (repo_dir, ref, expect) must still load,
// warn, and be written back clean.
func TestLegacyFieldsLoadAndAreDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lab.yaml")
	old := `name: old
switch: da-lab
subnet: 10.10.20.0/24
repo_dir: C:\src\directory-auditor
ref: main
work_dir: work
machines:
  - name: DC1
    kind: alt-dc
    domain: alt.test
    ip: 10.10.20.10
    expect: testdata/lab/samba-expect.yaml
`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Load(path)
	if err != nil {
		t.Fatalf("legacy file should load: %v", err)
	}
	if len(l.Warnings()) != 2 {
		t.Errorf("want 2 warnings (repo_dir/ref, expect), got %q", l.Warnings())
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, gone := range []string{"repo_dir", "ref:", "expect"} {
		if strings.Contains(string(b), gone) {
			t.Errorf("saved file still has %q:\n%s", gone, b)
		}
	}
	if want := filepath.Join(dir, "work"); l.Work() != want {
		t.Errorf("relative work_dir should resolve next to the lab file: %q, want %q", l.Work(), want)
	}
}

// An empty, unsaved lab must not pass the gate for any VM action — this is
// the screenshot bug: Build ran with nothing configured.
func TestGateBlocksUnconfiguredLab(t *testing.T) {
	l := NewDefault(filepath.Join(t.TempDir(), "lab.yaml"))
	cs := Checks(context.Background(), l, Creds{})
	if Ready(cs) {
		t.Fatal("a fresh lab must not be ready")
	}
	for _, a := range []string{"create", "provision", "up", "reset"} {
		if err := gate(cs, a, nil); err == nil {
			t.Errorf("gate(%s) should refuse on an unconfigured lab", a)
		}
	}
	ids := map[string]bool{}
	for _, c := range cs {
		ids[c.ID] = true
	}
	for _, want := range []string{"host", "saved", "valid", "targets", "creds"} {
		if !ids[want] {
			t.Errorf("checklist is missing %q", want)
		}
	}
}

// The host address (.1) is reserved for the host vNIC on the lab switch.
func TestHostAddressReserved(t *testing.T) {
	l := &Lab{Subnet: "10.10.30.0/24", Machines: []Machine{{Name: "dc", Kind: "windows-dc", Domain: "corp.test", IP: "10.10.30.1"}}}
	l.defaults()
	if err := l.Validate(); err == nil || !strings.Contains(err.Error(), "reserved for the host") {
		t.Fatalf("want host-address error, got %v", err)
	}
	if ip, p := hostAddr("10.10.30.0/24"); ip != "10.10.30.1" || p != 24 {
		t.Errorf("hostAddr = %s/%d", ip, p)
	}
}

// Each LAB_* variable has one meaning. The old code passed the DNS domain as
// LAB_DOMAIN, which Samba used as the NetBIOS name; the base DN never reached
// the populate script.
func TestVarsContract(t *testing.T) {
	m := &Machine{Name: "DC-01", Domain: "alt.test", IP: "10.10.20.10", Prefix: 24}
	v := Vars(m, Creds{AdminPassword: "a&b", AuditPassword: "c'd"})
	want := map[string]string{
		"LABCTL_LAB": "1", "LAB_DNS_DOMAIN": "alt.test", "LAB_REALM": "ALT.TEST",
		"LAB_NETBIOS": "ALT", "LAB_BASEDN": "DC=alt,DC=test", "LAB_HOST": "dc-01",
		"LAB_AUDIT_USER": "audit", "LAB_AUDIT_PASSWORD": "c'd",
	}
	for k, w := range want {
		if v[k] != w {
			t.Errorf("%s = %q, want %q", k, v[k], w)
		}
	}
	pre := exportPreamble(v)
	if !strings.Contains(pre, `export LAB_AUDIT_PASSWORD='c'\''d'`) {
		t.Errorf("password not shell-quoted safely:\n%s", pre)
	}
}

// Every Linux plugin's scripts are embedded and guarded; none still refers
// to the Directory Auditor repo or its guard variable.
func TestEmbeddedContent(t *testing.T) {
	for _, k := range Kinds() {
		p, _ := plugin(k.Kind)
		for _, s := range []string{p.ProvisionScript, p.PopulateScript} {
			if s == "" {
				continue
			}
			b, err := content.Read(s)
			if err != nil {
				t.Fatalf("%s: %s not embedded: %v", k.Kind, s, err)
			}
			if !strings.Contains(string(b), "LABCTL_LAB") {
				t.Errorf("%s: %s has no LABCTL_LAB guard", k.Kind, s)
			}
			if strings.Contains(string(b), "DIRAUDITOR_LAB") {
				t.Errorf("%s: %s still uses DIRAUDITOR_LAB", k.Kind, s)
			}
		}
	}
	root, err := content.Extract(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"windows/provision.ps1", "windows/populate.ps1"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(f))); err != nil {
			t.Errorf("extract missed %s", f)
		}
	}
}

// lab-info.json describes directories, never carries passwords.
func TestLabInfo(t *testing.T) {
	l := &Lab{Name: "mix", Switch: "lc-lab", Subnet: "10.10.30.0/24", path: filepath.Join(t.TempDir(), "lab.yaml"), Machines: []Machine{
		{Name: "W25", Kind: "windows-dc", Domain: "corp.test", IP: "10.10.30.10"},
		{Name: "ALT", Kind: "alt-domen", Domain: "alt.test", IP: "10.10.30.11"},
		{Name: "IPA", Kind: "ald-pro", Domain: "ipa.test", IP: "10.10.30.12"},
		{Name: "scan", Kind: "linux-scanner", IP: "10.10.30.20"},
	}}
	l.defaults()
	cr := Creds{AdminPassword: "SuperSecret1!", AuditPassword: "AuditSecret2!"}
	info := BuildInfo(context.Background(), l, cr)
	if info.Schema != LabInfoSchema || len(info.Directories) != 3 || info.Scanner == nil {
		t.Fatalf("unexpected info: %+v", info)
	}
	if d := info.Directories[0]; d.Type != "ad" || d.Endpoints.LDAPS != "" || d.Accounts.Admin != "Administrator@CORP.TEST" {
		t.Errorf("AD entry wrong: %+v", d)
	}
	if d := info.Directories[2]; d.Type != "freeipa" || d.Accounts.Admin != "admin@IPA.TEST" || d.Endpoints.LDAPS == "" {
		t.Errorf("FreeIPA entry wrong: %+v", d)
	}
	p, err := WriteInfo(context.Background(), l, cr)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "SuperSecret1!") || strings.Contains(string(b), "AuditSecret2!") {
		t.Fatal("lab-info.json must not contain passwords")
	}
	var round LabInfo
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	if info.Network.HostIP != "10.10.30.1" {
		t.Errorf("host ip = %q", info.Network.HostIP)
	}
}

// An ampersand in the admin password must not break the answer file.
func TestAutounattendEscapesPassword(t *testing.T) {
	x := winAutounattend(&Machine{Name: "W25", Domain: "corp.test"}, `P&ss<word>`)
	if strings.Contains(x, "P&ss<") || !strings.Contains(x, "P&amp;ss&lt;word&gt;") {
		t.Errorf("password not escaped in autounattend.xml")
	}
}

// The shipped example must load and validate as-is.
func TestExampleLoads(t *testing.T) {
	l, err := Load(filepath.Join("..", "..", "lab.example.yaml"))
	if err != nil {
		t.Fatalf("lab.example.yaml: %v", err)
	}
	if len(l.Warnings()) != 0 {
		t.Errorf("example should have no legacy fields: %q", l.Warnings())
	}
}
