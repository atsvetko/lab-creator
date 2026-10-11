package labctl

import (
	"path/filepath"
	"testing"
)

// TestSSHUserResolution covers the precedence learned from the live lab: a
// per-machine override wins, then the lab default, then root.
func TestSSHUserResolution(t *testing.T) {
	lab := &Lab{SSHUser: "altadmin"}
	cases := []struct {
		name string
		m    Machine
		want string
	}{
		{"lab default applies", Machine{}, "altadmin"},
		{"machine override wins", Machine{SSHUser: "labadmin"}, "labadmin"},
	}
	for _, c := range cases {
		if got := c.m.sshUser(lab); got != c.want {
			t.Errorf("%s: sshUser = %q, want %q", c.name, got, c.want)
		}
	}
	// With no lab default and no override, root is the fallback.
	if got := (&Machine{}).sshUser(&Lab{}); got != "root" {
		t.Errorf("fallback: sshUser = %q, want root", got)
	}
	// priv() only sudo-wraps a non-root login.
	if got := priv("root", "bash -s"); got != "bash -s" {
		t.Errorf("priv(root) = %q", got)
	}
	if got := priv("altadmin", "env X=1 bash -s"); got != "sudo env X=1 bash -s" {
		t.Errorf("priv(altadmin) = %q", got)
	}
}

// TestSubnetValidation covers the re-IP lesson: an IP outside the declared
// subnet is a config error caught before any VM is touched.
func TestSubnetValidation(t *testing.T) {
	base := func() *Lab {
		return &Lab{
			RepoDir: ".", Subnet: "10.10.20.0/24",
			Machines: []Machine{{Name: "dc", Kind: "windows-dc", Domain: "da.test", IP: "10.10.20.10"}},
		}
	}
	l := base()
	l.defaults()
	if err := l.Validate(); err != nil {
		t.Fatalf("in-subnet IP should pass: %v", err)
	}

	bad := base()
	bad.Machines[0].IP = "10.10.99.10"
	bad.defaults()
	if err := bad.Validate(); err == nil {
		t.Fatal("out-of-subnet IP should fail validation")
	}

	badcidr := base()
	badcidr.Subnet = "not-a-cidr"
	badcidr.defaults()
	if err := badcidr.Validate(); err == nil {
		t.Fatal("invalid subnet CIDR should fail validation")
	}

	// No subnet declared: IPs are not range-checked.
	none := base()
	none.Subnet = ""
	none.Machines[0].IP = "192.168.5.5"
	none.defaults()
	if err := none.Validate(); err != nil {
		t.Fatalf("no subnet means no range check: %v", err)
	}
}

// TestSaveLoadRoundTrip is what `labctl init` relies on: the scaffolded file
// must load back and keep the new fields.
func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lab.yaml")
	lab := &Lab{
		Name: "w2025", Switch: "da-lab", Subnet: "10.10.30.0/24", RepoDir: ".",
		SSHUser: "altadmin", path: path,
		Machines: []Machine{{Name: "w2025", Kind: "windows-dc", ISO: `D:\iso\win2025.iso`, Domain: "da.test", IP: "10.10.30.10"}},
	}
	if err := lab.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SSHUser != "altadmin" {
		t.Errorf("lab ssh_user not preserved: %q", got.SSHUser)
	}
	if got.Machines[0].ForestMode != "Win2016" {
		t.Errorf("windows-dc forest_mode default lost: %q", got.Machines[0].ForestMode)
	}
	if got.Machines[0].ISO != `D:\iso\win2025.iso` {
		t.Errorf("iso path mangled: %q", got.Machines[0].ISO)
	}
}
