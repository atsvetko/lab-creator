package labctl

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

// Exit codes. CI jobs branch on these, so they are part of the contract.
const (
	ExitOK          = 0
	ExitFailed      = 1 // an action failed
	ExitUsage       = 2 // bad arguments or invalid lab file
	ExitHostNotOK   = 3 // Setup requirements not met (host, rights, switch, credentials)
	ExitProvisionKO = 5 // create/provision/snapshot failed during `up`
)

// RunCLI dispatches the headless sub-commands. It returns a process exit code,
// or -1 when there is no sub-command and the caller should start the web UI.
//
//	labctl init  --name w2025 --iso D:\iso\win2025.iso --domain corp.test --ip 10.10.30.10
//	labctl doctor                     # the Setup checklist
//	labctl up                         # create + provision + snapshot everything
//	labctl reset --all                # back to the baseline
//	labctl info                       # print lab-info.json
func RunCLI(args []string) int {
	if len(args) == 0 {
		return -1
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "init":
		return cliInit(rest)
	case "doctor":
		return cliDoctor(rest)
	case "create", "provision", "snapshot", "reset", "start", "stop":
		return cliMachine(cmd, rest)
	case "up":
		return cliUp(rest)
	case "info":
		return cliInfo(rest)
	case "kinds":
		return cliKinds()
	case "serve":
		return -1
	case "-h", "--help", "help":
		cliUsage(os.Stdout)
		return ExitOK
	default:
		fmt.Fprintf(os.Stderr, "labctl: unknown command %q\n\n", cmd)
		cliUsage(os.Stderr)
		return ExitUsage
	}
}

func cliUsage(w *os.File) {
	fmt.Fprint(w, `labctl — builds directory test labs on Hyper-V

Usage:
  labctl [--lab FILE] [--no-browser]           start the web UI (default)
  labctl init     <flags>                      write a lab file (no scripts needed)
  labctl doctor   [--lab FILE]                 show the Setup checklist
  labctl up       [--lab FILE]                 create + provision + snapshot every machine
  labctl create   <machine|--all> [--lab FILE] create the VM(s) from their ISOs
  labctl provision <machine|--all>             promote the DC(s) and seed weak objects
  labctl snapshot <machine|--all>              take the baseline checkpoint
  labctl reset    <machine|--all>              restore the baseline checkpoint
  labctl start|stop <machine|--all>            power the VM(s) on/off
  labctl info     [--lab FILE] [--out PATH]    print or write lab-info.json
  labctl kinds                                 list supported guest kinds

The lab file defaults to lab.yaml in the current folder, else next to labctl.

init flags:
  --name NAME         lab and DC VM name (required)
  --iso PATH          install ISO, e.g. the Windows Server 2025 ISO (required)
  --kind KIND         DC kind (default windows-dc; see 'labctl kinds')
  --domain DOMAIN     DNS domain of the directory (default corp.test)
  --ip IP             DC static IP on the lab switch (default 10.10.30.10)
  --subnet CIDR       lab subnet (default derived from --ip as /24; .1 is the host)
  --switch NAME       Hyper-V internal switch (default lc-lab; created if missing)
  --forest-mode M     AD functional level: Win2016 | Win2025 (default Win2016)
  --cpu N / --mem-gb N / --disk-gb N   VM sizing (defaults 2 / 4 / 60)
  --scanner           also add a non-joined Linux workstation for testing tools
  --scanner-iso PATH  its install ISO (required with --scanner)
  --scanner-ip IP     its static IP (default: DC IP +1)
  --ssh-user USER     SSH login for Linux guests; vendor DCs disable root,
                      so use the sudo-capable admin (e.g. altadmin)
  --lab FILE          where to write the lab file (default lab.yaml)

Credentials come from the environment, never the command line or the lab file:
  LAB_ADMIN_PASSWORD  domain administrator password set on every DC
  LAB_AUDIT_PASSWORD  password of the ordinary account testers bind as
  LAB_AUDIT_USER      its name (default audit)

Exit codes: 0 ok · 1 action failed · 2 usage or invalid lab file ·
            3 Setup not ready · 5 provisioning failed during up
`)
}

func cliKinds() int {
	fmt.Println("supported guest kinds:")
	for _, k := range Kinds() {
		role := k.Directory
		if k.Scanner {
			role = "scanner"
		}
		fmt.Printf("  %-14s %-8s %-8s %s\n", k.Kind, k.OS, role, k.Label)
	}
	return ExitOK
}

// DefaultLabPath picks the lab file when none is given: lab.yaml in the
// current folder if it exists, else next to the labctl binary if it exists,
// else lab.yaml in the current folder (where a new one will be written).
func DefaultLabPath() string {
	const name = "lab.yaml"
	if _, err := os.Stat(name); err == nil {
		return name
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

func labFlag(fs *flag.FlagSet) *string {
	return fs.String("lab", "", "lab file (default: lab.yaml here, else next to labctl)")
}

func labPathOr(p string) string {
	if p == "" {
		return DefaultLabPath()
	}
	return p
}

// cliInit writes a lab file from flags. It validates; it touches no VM.
func cliInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	var (
		labPath    = fs.String("lab", "lab.yaml", "")
		name       = fs.String("name", "", "")
		iso        = fs.String("iso", "", "")
		kind       = fs.String("kind", "windows-dc", "")
		domain     = fs.String("domain", "corp.test", "")
		ip         = fs.String("ip", "10.10.30.10", "")
		subnet     = fs.String("subnet", "", "")
		sw         = fs.String("switch", "lc-lab", "")
		forestMode = fs.String("forest-mode", "", "")
		cpu        = fs.Int("cpu", 0, "")
		memGB      = fs.Int("mem-gb", 0, "")
		diskGB     = fs.Int("disk-gb", 0, "")
		scanner    = fs.Bool("scanner", false, "")
		scannerISO = fs.String("scanner-iso", "", "")
		scannerIP  = fs.String("scanner-ip", "", "")
		sshUser    = fs.String("ssh-user", "", "")
		force      = fs.Bool("force", false, "overwrite an existing lab file")
	)
	fs.Usage = func() { cliUsage(os.Stderr) }
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *name == "" || *iso == "" {
		fmt.Fprintln(os.Stderr, "labctl init: --name and --iso are required")
		return ExitUsage
	}
	p, ok := plugin(*kind)
	if !ok || p.Scanner {
		fmt.Fprintf(os.Stderr, "labctl init: --kind %q is not a directory server (see 'labctl kinds')\n", *kind)
		return ExitUsage
	}
	if !*force {
		if _, err := os.Stat(*labPath); err == nil {
			fmt.Fprintf(os.Stderr, "labctl init: %s already exists (use --force to overwrite)\n", *labPath)
			return ExitUsage
		}
	}
	if *subnet == "" {
		*subnet = defaultSubnet(*ip)
	}
	dc := Machine{
		Name: *name, Kind: p.Kind, ISO: *iso, Domain: *domain, IP: *ip,
		CPU: *cpu, MemGB: *memGB, DiskGB: *diskGB, ForestMode: *forestMode,
	}
	lab := &Lab{Name: *name, Switch: *sw, Subnet: *subnet, SSHUser: *sshUser, Machines: []Machine{dc}}
	if *scanner {
		if *scannerISO == "" {
			fmt.Fprintln(os.Stderr, "labctl init: --scanner needs --scanner-iso")
			return ExitUsage
		}
		sip := *scannerIP
		if sip == "" {
			sip = nextIP(*ip)
		}
		lab.Machines = append(lab.Machines, Machine{Name: *name + "-scanner", Kind: "linux-scanner", ISO: *scannerISO, IP: sip})
	}
	lab.path = *labPath
	if err := lab.Save(); err != nil {
		fmt.Fprintln(os.Stderr, "labctl init:", err)
		return ExitUsage
	}
	fmt.Printf("wrote %s\n", *labPath)
	fmt.Printf("  %s %q on %s (switch %s), domain %s, subnet %s\n", p.Kind, *name, *ip, *sw, *domain, *subnet)
	fmt.Println("next: set LAB_ADMIN_PASSWORD and LAB_AUDIT_PASSWORD, run 'labctl doctor', then 'labctl up'")
	return ExitOK
}

// cliDoctor prints the Setup checklist; exit 3 when a requirement is unmet.
func cliDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	labPath := labFlag(fs)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	lab, code := loadOrNew(labPathOr(*labPath))
	if lab == nil {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs := Checks(ctx, lab, credsFromEnv())
	fmt.Printf("lab: %s\n", lab.Path())
	for _, w := range lab.Warnings() {
		fmt.Printf("  note: %s\n", w)
	}
	for _, c := range cs {
		mark := "ok  "
		switch {
		case !c.OK && c.Required:
			mark = "FAIL"
		case !c.OK:
			mark = "warn"
		}
		fmt.Printf("  [%s] %s", mark, c.Label)
		if c.Detail != "" {
			fmt.Printf(" — %s", c.Detail)
		}
		fmt.Println()
	}
	if !Ready(cs) {
		fmt.Println("not ready.")
		return ExitHostNotOK
	}
	fmt.Println("ready.")
	return ExitOK
}

// cliMachine runs one per-machine action (optionally across --all).
func cliMachine(action string, args []string) int {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	labPath := labFlag(fs)
	all := fs.Bool("all", false, "apply to every machine")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	lab, code := loadLab(labPathOr(*labPath))
	if lab == nil {
		return code
	}
	targets, err := selectMachines(lab, *all, fs.Args(), action)
	if err != nil {
		fmt.Fprintln(os.Stderr, "labctl "+action+":", err)
		return ExitUsage
	}
	cr := credsFromEnv()
	cs := checksNow(lab, cr)
	var failed int
	for _, m := range targets {
		if err := gate(cs, action, m); err != nil {
			fmt.Fprintf(os.Stderr, "labctl %s %s: %v\n", action, m.Name, err)
			return ExitHostNotOK
		}
		err := runSync(action+" "+m.Name, func(ctx context.Context, j *Job) error {
			return machineAction(ctx, j, lab, m, action, cr)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "labctl %s %s: %v\n", action, m.Name, err)
			failed++
		}
	}
	if action == "provision" || action == "snapshot" || action == "reset" {
		writeInfoCLI(lab, cr)
	}
	if failed > 0 {
		return ExitFailed
	}
	return ExitOK
}

// cliUp creates, provisions and snapshots every machine: directory servers
// first, then the scanner.
func cliUp(args []string) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	labPath := labFlag(fs)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	lab, code := loadLab(labPathOr(*labPath))
	if lab == nil {
		return code
	}
	cr := credsFromEnv()
	cs := checksNow(lab, cr)
	if err := gate(cs, "up", nil); err != nil {
		fmt.Fprintln(os.Stderr, "labctl up:", err)
		fmt.Fprintln(os.Stderr, "run 'labctl doctor' for the full checklist")
		return ExitHostNotOK
	}
	order := make([]*Machine, 0, len(lab.Machines))
	for i := range lab.Machines {
		if !lab.Machines[i].isScanner() {
			order = append(order, &lab.Machines[i])
		}
	}
	for i := range lab.Machines {
		if lab.Machines[i].isScanner() {
			order = append(order, &lab.Machines[i])
		}
	}
	for _, m := range order {
		fmt.Printf("=== %s (%s) ===\n", m.Name, m.Kind)
		for _, step := range []string{"create", "provision", "snapshot"} {
			if m.isScanner() && step == "snapshot" {
				continue
			}
			if err := gate(cs, step, m); err != nil {
				fmt.Fprintf(os.Stderr, "labctl up %s %s: %v\n", step, m.Name, err)
				return ExitHostNotOK
			}
			err := runSync(step+" "+m.Name, func(ctx context.Context, j *Job) error {
				return machineAction(ctx, j, lab, m, step, cr)
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "labctl up %s %s: %v\n", step, m.Name, err)
				return ExitProvisionKO
			}
		}
	}
	writeInfoCLI(lab, cr)
	fmt.Println("lab is up.")
	return ExitOK
}

// cliInfo prints lab-info.json, or writes it to --out.
func cliInfo(args []string) int {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	labPath := labFlag(fs)
	out := fs.String("out", "", "write to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	lab, code := loadLab(labPathOr(*labPath))
	if lab == nil {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := json.MarshalIndent(BuildInfo(ctx, lab, credsFromEnv()), "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "labctl info:", err)
		return ExitFailed
	}
	b = append(b, '\n')
	if *out == "" {
		_, _ = os.Stdout.Write(b)
		return ExitOK
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "labctl info:", err)
		return ExitFailed
	}
	fmt.Println("wrote", *out)
	return ExitOK
}

// --- helpers ----------------------------------------------------------------

func checksNow(lab *Lab, cr Creds) []Check {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return Checks(ctx, lab, cr)
}

func writeInfoCLI(lab *Lab, cr Creds) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if p, err := WriteInfo(ctx, lab, cr); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not write lab-info.json:", err)
	} else {
		fmt.Println("lab description:", p)
	}
}

func loadLab(path string) (*Lab, int) {
	lab, err := Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "hint: run 'labctl init …' or open the web UI to create", path)
		}
		return nil, ExitUsage
	}
	for _, w := range lab.Warnings() {
		fmt.Fprintln(os.Stderr, "note:", w)
	}
	return lab, ExitOK
}

// loadOrNew loads the lab, or returns an unsaved default when the file does
// not exist yet (doctor still reports on the host).
func loadOrNew(path string) (*Lab, int) {
	lab, err := Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewDefault(path), ExitOK
	}
	if err != nil && lab == nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return nil, ExitUsage
	}
	return lab, ExitOK
}

func credsFromEnv() Creds {
	return Creds{
		AdminPassword: os.Getenv("LAB_ADMIN_PASSWORD"),
		AuditPassword: os.Getenv("LAB_AUDIT_PASSWORD"),
		AuditUser:     orDefault(os.Getenv("LAB_AUDIT_USER"), "audit"),
	}
}

// selectMachines resolves the machine arguments for a per-machine command.
func selectMachines(l *Lab, all bool, names []string, action string) ([]*Machine, error) {
	if all {
		var out []*Machine
		for i := range l.Machines {
			m := &l.Machines[i]
			if m.isScanner() && (action == "snapshot" || action == "reset") {
				continue // the scanner has no directory baseline
			}
			out = append(out, m)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no applicable machines")
		}
		return out, nil
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("name a machine or pass --all")
	}
	var out []*Machine
	for _, n := range names {
		m := l.Machine(n)
		if m == nil {
			return nil, fmt.Errorf("no such machine %q", n)
		}
		out = append(out, m)
	}
	return out, nil
}

// runSync runs a job function to completion, streaming its log to stdout, and
// returns the job's error. Ctrl+C cancels the underlying context.
func runSync(title string, fn func(ctx context.Context, j *Job) error) error {
	fmt.Printf("── %s ──\n", title)
	j := &Job{ID: "cli", Title: title, Status: "running", Start: time.Now()}
	hist, ch, _ := j.Subscribe()
	for _, l := range hist {
		fmt.Println(l)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		err := fn(ctx, j)
		errc <- err
		j.finish(err)
	}()
	for l := range ch {
		fmt.Println(l)
	}
	return <-errc
}

// defaultSubnet turns an IPv4 host address into its /24 network string.
func defaultSubnet(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ""
	}
	return fmt.Sprintf("%s.%s.%s.0/24", parts[0], parts[1], parts[2])
}

// nextIP returns the address one above the given IPv4 host (last octet +1).
func nextIP(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ip
	}
	var last int
	if _, err := fmt.Sscanf(parts[3], "%d", &last); err != nil || last >= 254 {
		return ip
	}
	return fmt.Sprintf("%s.%s.%s.%d", parts[0], parts[1], parts[2], last+1)
}
