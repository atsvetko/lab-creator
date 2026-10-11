// Command labctl is the Directory Auditor lab control panel. It runs on the
// Hyper-V host and either serves a local web UI or drives the lab from the
// command line: it builds the auditor from a local clone, creates and
// provisions the lab VMs (Windows AD and Samba/Альт Домен DCs) from ISOs you
// supply, and deploys the freshly built binary to scan them.
//
// It is a separate tool from the read-only auditor and is never linked into it.
//
//	labctl [--lab lab.yaml] [--no-browser]   # web UI (default)
//	labctl init --name w2025 --iso <win2025.iso> --domain da.test --ip 10.10.30.10
//	labctl up                                # build + create + provision + snapshot
//	labctl help                              # all sub-commands
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"runtime"

	"github.com/atsvetko/lab-creator/internal/labctl"
)

func main() {
	// A sub-command (init, build, up, create, provision, scan, …) runs headless
	// and exits. Anything else — no args, or just the server flags — falls
	// through to the web UI, so the bare `labctl` invocation is unchanged.
	// `serve` is the explicit name for the UI: strip it and parse its flags.
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		os.Args = append(os.Args[:1:1], args[1:]...) // strip "serve"; parse its flags below
	} else if len(args) > 0 && args[0][0] != '-' {
		os.Exit(labctl.RunCLI(args))
	}

	labPath := flag.String("lab", "lab.yaml", "lab definition file")
	noBrowser := flag.Bool("no-browser", false, "do not open a browser; print the address only")
	flag.Parse()

	lab, err := labctl.Load(*labPath)
	fresh := false
	if err != nil {
		// No lab file yet: open the UI anyway with a default, empty lab so the
		// user can build it in the config editor (the first Save writes the
		// file). Any other error — a malformed or invalid file — is still fatal.
		if errors.Is(err, fs.ErrNotExist) {
			lab = labctl.NewDefault(*labPath)
			fresh = true
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprintln(os.Stderr, "hint: fix", *labPath, "or run `labctl init …` to scaffold it")
			os.Exit(1)
		}
	}
	_, url, err := labctl.Serve(lab)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println("Directory Auditor lab control panel")
	if fresh {
		fmt.Println("  lab:", lab.Path(), "(new — nothing saved yet; build it on the Config screen)")
	} else {
		fmt.Println("  lab:", lab.Path())
	}
	fmt.Println("  open:", url)
	if ok, msg := labctl.HostOK(); !ok {
		fmt.Println("  note:", msg)
	}
	if !*noBrowser {
		openBrowser(url)
	}
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
	fmt.Println("\nstopped.")
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
