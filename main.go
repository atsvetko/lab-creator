// Command labctl builds directory test labs on Hyper-V. It serves a local web
// UI or runs headless: it creates VMs from ISOs you supply, promotes them to
// domain controllers (Windows AD, Samba AD DC, FreeIPA), seeds deliberately
// weak objects, checkpoints a baseline, and describes the lab in
// lab-info.json for whatever tool you test with. Its provisioning scripts are
// embedded; nothing else is needed on the host.
//
//	labctl [--lab lab.yaml] [--no-browser]   # web UI (default)
//	labctl init --name w2025 --iso <win2025.iso> --domain corp.test --ip 10.10.30.10
//	labctl doctor                            # Setup checklist
//	labctl up                                # create + provision + snapshot
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

	labPath := flag.String("lab", "", "lab file (default: lab.yaml here, else next to labctl)")
	noBrowser := flag.Bool("no-browser", false, "do not open a browser; print the address only")
	flag.Parse()
	if *labPath == "" {
		*labPath = labctl.DefaultLabPath()
	}

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
			pauseIfDoubleClicked()
			os.Exit(1)
		}
	}
	_, url, err := labctl.Serve(lab)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		pauseIfDoubleClicked()
		os.Exit(1)
	}
	fmt.Println("lab-creator control panel")
	for _, w := range lab.Warnings() {
		fmt.Println("  note:", w)
	}
	if fresh {
		fmt.Println("  lab:", lab.Path(), "(new — nothing saved yet; add machines on the Config screen)")
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
