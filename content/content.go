// Package content holds the provisioning scripts lab-creator runs inside lab
// guests: promoting a domain controller and seeding deliberately weak objects.
// They are embedded in the labctl binary, so a lab needs no source checkout.
//
// Every script refuses to run unless LABCTL_LAB=1 is set, and takes its
// parameters from LAB_* environment variables (see Vars in package labctl).
package content

import (
	"bytes"
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

// Version identifies this set of scripts. Bump it when any script changes, so
// extracted copies on disk are refreshed and labs can record what built them.
const Version = "2026.10.11-1"

//go:embed windows/*.ps1 samba/*.sh freeipa/*.sh aldpro/*.sh
var FS embed.FS

// Read returns one script by its content-relative path, e.g. "samba/populate.sh".
func Read(path string) ([]byte, error) { return FS.ReadFile(path) }

// Extract writes the scripts to dir/<Version>/ (only files that are missing or
// differ) and returns that directory. Windows provisioning reads them from
// there; keeping them on disk also lets an administrator inspect what runs.
func Extract(dir string) (string, error) {
	root := filepath.Join(dir, Version)
	err := fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := FS.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, filepath.FromSlash(p))
		if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, b) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	return root, err
}
