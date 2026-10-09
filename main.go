// Command labctl is the Directory Auditor lab control panel. It runs on the
// Hyper-V host and serves a local web UI that builds the auditor from a local
// clone, creates and provisions the lab VMs (Windows AD and Samba/Альт Домен
// DCs) from ISOs you supply, and deploys the freshly built binary to scan them.
//
// It is a separate tool from the read-only auditor and is never linked into it.
//
//	labctl [--lab lab.yaml] [--no-browser]
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"

	"github.com/atsvetko/lab-creator/internal/labctl"
)

func main() {
	labPath := flag.String("lab", "lab.yaml", "lab definition file")
	noBrowser := flag.Bool("no-browser", false, "do not open a browser; print the address only")
	flag.Parse()

	lab, err := labctl.Load(*labPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, "hint: copy lab.example.yaml to", *labPath, "and edit it")
		os.Exit(1)
	}
	_, url, err := labctl.Serve(lab)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println("Directory Auditor lab control panel")
	fmt.Println("  lab:", lab.Path())
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
