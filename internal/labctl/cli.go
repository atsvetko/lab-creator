package labctl

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"
)

// RunCLI dispatches the headless sub-commands. It returns a process exit code.
// With no sub-command (or "serve") the caller falls back to the web UI, so the
// bare `labctl` invocation is unchanged. Everything a lab needs — scaffolding
// the definition, building the auditor, creating/provisioning/scanning the VMs
// — is reachable from the command line, so a Windows Server 2025 (or Samba)
// lab can be stood up end to end without hand-writing lab.yaml or any scripts.
//
//	labctl init  --name w2025 --iso D:\iso\win2025.iso --domain da.test --ip 10.10.30.10
//	labctl up                         # build + create + provision + snapshot everything
//	labctl scan  --all                # scan every target from the scanner VM
func RunCLI(args []string) int {
	if len(args) == 0 {
		return -1 // signal to main: no sub-command, run the web UI
	}
	cmd := args[0]
	rest := args[1:]
	switch cmd {
	case "init":
		return cliInit(rest)
	case "build":
		return cliBuild(rest)
	case "create", "provision", "snapshot", "scan", "start", "stop":
		return cliMachine(cmd, rest)
	case "up":
		return cliUp(rest)
	case "kinds":
		return cliKinds()
	case "serve":
		return -1 // handled by main
	case "-h", "--help", "help":
		cliUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "labctl: unknown command %q\n\n", cmd)
		cliUsage(os.Stderr)
		return 2
	}
}

func cliUsage(w *os.File) {
	fmt.Fprint(w, `labctl — Directory Auditor lab control panel

Usage:
  labctl [--lab lab.yaml] [--no-browser]        start the web UI (default)
  labctl init   <flags>                         scaffold a lab.yaml (no scripts needed)
  labctl build  [--ref REF] [--lab F]           build the auditor for the lab
  labctl up     [--lab F] [--scan]              build + create + provision + snapshot all
  labctl create   <machine|--all> [--lab F]     create the VM(s)
  labctl provision<machine|--all> [--lab F]     promote the DC and seed fixtures
  labctl snapshot <machine|--all> [--lab F]     take the 'vuln-baseline' checkpoint
  labctl scan     <machine|--all> [--lab F]     scan target(s) from the scanner VM
  labctl start|stop <machine|--all> [--lab F]   power the VM(s) on/off
  labctl kinds                                  list supported guest kinds

init flags:
  --name NAME         lab and DC VM name (required)
  --iso PATH          install ISO, e.g. the Windows Server 2025 ISO (required)
  --kind KIND         DC kind (default windows-dc; see 'labctl kinds')
  --domain DOMAIN     AD/Kerberos domain (default da.test)
  --ip IP             DC static IP on the lab switch (default 10.10.30.10)
  --subnet CIDR       lab subnet (default derived from --ip as /24)
  --switch NAME       Hyper-V internal switch (default da-lab)
  --repo PATH         local directory-auditor clone to build (default ../directory-auditor)
  --forest-mode M     AD forest/domain functional level (default Win2016; the OS is set by the ISO)
  --cpu N / --mem-gb N / --disk-gb N   VM sizing (defaults 2 / 4 / 60)
  --scanner           also add a non-joined Linux scanner VM (needed to scan)
  --scanner-iso PATH  scanner install ISO (required with --scanner)
  --scanner-ip IP     scanner static IP (default: DC IP +1)
  --ssh-user USER     default SSH login for Linux guests; vendor DCs disable
                      root, so set the sudo-capable admin here (e.g. altadmin)
  --lab FILE          where to write the definition (default lab.yaml)

Credentials are read from the environment, never the command line or the lab
file: LAB_ADMIN_PASSWORD (DC admin, used by create/provision) and
LAB_AUDIT_PASSWORD (the audit account the scan binds as).
`)
}

func cliKinds() int {
	fmt.Println("supported guest kinds:")
	for _, k := range Kinds() {
		role := "target"
		if k.Scanner {
			role = "scanner"
		}
		fmt.Printf("  %-14s %-8s %-7s %s\n", k.Kind, k.OS, role, k.Label)
	}
	return 0
}

// cliInit scaffolds a lab.yaml from flags — the "create a lab without writing
// scripts" path. It writes, defaults and validates the file; it touches no VM.
func cliInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	var (
		labPath    = fs.String("lab", "lab.yaml", "")
		name       = fs.String("name", "", "")
		iso        = fs.String("iso", "", "")
		kind       = fs.String("kind", "windows-dc", "")
		domain     = fs.String("domain", "da.test", "")
		ip         = fs.String("ip", "10.10.30.10", "")
		subnet     = fs.String("subnet", "", "")
		sw         = fs.String("switch", "da-lab", "")
		repo       = fs.String("repo", "../directory-auditor", "")
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
		return 2
	}
	if *name == "" || *iso == "" {
		fmt.Fprintln(os.Stderr, "labctl init: --name and --iso are required")
		return 2
	}
	if _, ok := plugin(*kind); !ok {
		fmt.Fprintf(os.Stderr, "labctl init: unknown --kind %q (see 'labctl kinds')\n", *kind)
		return 2
	}
	if !*force {
		if _, err := os.Stat(*labPath); err == nil {
			fmt.Fprintf(os.Stderr, "labctl init: %s already exists (use --force to overwrite)\n", *labPath)
			return 2
		}
	}
	if *subnet == "" {
		*subnet = defaultSubnet(*ip)
	}
	dc := Machine{
		Name: *name, Kind: *kind, ISO: *iso, Domain: *domain, IP: *ip,
		CPU: *cpu, MemGB: *memGB, DiskGB: *diskGB, ForestMode: *forestMode,
	}
	if p, _ := plugin(*kind); p.DefaultExpect != "" {
		dc.Expect = p.DefaultExpect
	}
	lab := &Lab{
		Name: *name, Switch: *sw, Subnet: *subnet, RepoDir: *repo, Ref: "main",
		SSHUser: *sshUser, Machines: []Machine{dc},
	}
	if *scanner {
		if *scannerISO == "" {
			fmt.Fprintln(os.Stderr, "labctl init: --scanner needs --scanner-iso")
			return 2
		}
		sip := *scannerIP
		if sip == "" {
			sip = nextIP(*ip)
		}
		lab.Machines = append(lab.Machines, Machine{
			Name: *name + "-scanner", Kind: "linux-scanner", ISO: *scannerISO, IP: sip,
		})
	}
	// Write via Save, which applies defaults and validates; set the path first.
	lab.path = *labPath
	if err := lab.Save(); err != nil {
		fmt.Fprintln(os.Stderr, "labctl init:", err)
		return 1
	}
	fmt.Printf("wrote %s\n", *labPath)
	fmt.Printf("  %s DC %q on %s (%s), domain %s, subnet %s\n", *kind, *name, *ip, *sw, *domain, *subnet)
	if *scanner {
		fmt.Printf("  scanner %s-scanner on %s\n", *name, lab.Machines[len(lab.Machines)-1].IP)
	} else {
		fmt.Println("  note: add a scanner (--scanner) to run scans against this DC")
	}
	fmt.Println("next: set LAB_ADMIN_PASSWORD (and LAB_AUDIT_PASSWORD for scans), then: labctl up")
	return 0
}

func cliBuild(args []string) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	labPath := fs.String("lab", "lab.yaml", "")
	ref := fs.String("ref", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	lab, code := loadLab(*labPath)
	if lab == nil {
		return code
	}
	s := &Server{lab: lab}
	err := runSync("build", func(ctx context.Context, j *Job) error {
		b, err := Build(ctx, j, lab, *ref)
		if err == nil {
			s.built = b
		}
		return err
	})
	return exit(err)
}

// cliMachine runs a single per-machine action (optionally across --all).
func cliMachine(action string, args []string) int {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	labPath := fs.String("lab", "lab.yaml", "")
	all := fs.Bool("all", false, "apply to every machine")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	lab, code := loadLab(*labPath)
	if lab == nil {
		return code
	}
	targets, err := selectMachines(lab, *all, fs.Args(), action)
	if err != nil {
		fmt.Fprintln(os.Stderr, "labctl "+action+":", err)
		return 2
	}
	cr := credsFromEnv()
	s := &Server{lab: lab, results: map[string]MachineResult{}, creds: cr}
	// scan/provision need an already-built binary on the scanner; build first.
	if action == "scan" {
		if code := prebuild(s, lab); code != 0 {
			return code
		}
	}
	var failed int
	for _, m := range targets {
		if err := runMachineAction(s, action, m, cr); err != nil {
			fmt.Fprintf(os.Stderr, "labctl %s %s: %v\n", action, m.Name, err)
			failed++
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// cliUp is the one-command path: build, then create → provision → snapshot
// every machine in dependency order (targets then scanner). With --scan it also
// scans and verifies each target afterwards.
func cliUp(args []string) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	labPath := fs.String("lab", "lab.yaml", "")
	doScan := fs.Bool("scan", false, "also scan and verify every target at the end")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	lab, code := loadLab(*labPath)
	if lab == nil {
		return code
	}
	if ok, why := HostOK(); !ok {
		fmt.Fprintln(os.Stderr, "labctl up: this needs the Hyper-V host —", why)
		return 1
	}
	cr := credsFromEnv()
	if cr.AdminPassword == "" {
		fmt.Fprintln(os.Stderr, "labctl up: set LAB_ADMIN_PASSWORD (the DC admin password)")
		return 2
	}
	s := &Server{lab: lab, results: map[string]MachineResult{}, creds: cr}
	if code := prebuild(s, lab); code != 0 {
		return code
	}
	for _, m := range lab.Machines {
		m := m
		fmt.Printf("=== %s (%s) ===\n", m.Name, m.Kind)
		for _, step := range []string{"create", "provision", "snapshot"} {
			if m.isScanner() && step == "snapshot" {
				continue // the scanner has no directory to baseline
			}
			if err := runMachineAction(s, step, &m, cr); err != nil {
				fmt.Fprintf(os.Stderr, "labctl up %s %s: %v\n", step, m.Name, err)
				return 1
			}
		}
	}
	if *doScan {
		if cr.AuditPassword == "" {
			fmt.Fprintln(os.Stderr, "labctl up --scan: set LAB_AUDIT_PASSWORD")
			return 2
		}
		for i := range lab.Machines {
			m := &lab.Machines[i]
			if m.isScanner() {
				continue
			}
			if err := runMachineAction(s, "scan", m, cr); err != nil {
				fmt.Fprintf(os.Stderr, "labctl up scan %s: %v\n", m.Name, err)
				return 1
			}
		}
	}
	fmt.Println("lab is up.")
	return 0
}

// runMachineAction executes one action synchronously, streaming to stdout.
func runMachineAction(s *Server, action string, m *Machine, cr Creds) error {
	title := action + " " + m.Name
	return runSync(title, func(ctx context.Context, j *Job) error {
		switch action {
		case "create":
			iso, err := answerISO(ctx, j, s.lab, m, cr)
			if err != nil {
				return err
			}
			return createVM(ctx, j, s.lab, m, iso)
		case "start":
			return startVM(ctx, j, m.Name)
		case "stop":
			return stopVM(ctx, j, m.Name)
		case "provision":
			if m.isScanner() {
				return provisionScanner(ctx, j, s.lab, m, cr)
			}
			if err := s.provision(ctx, j, m, cr); err != nil {
				return err
			}
			return waitLDAP(ctx, j, m.IP, 12*time.Minute)
		case "snapshot":
			return snapshotVM(ctx, j, m.Name, "vuln-baseline")
		case "scan":
			if m.isScanner() {
				return fmt.Errorf("%s is the scanner, not a scan target", m.Name)
			}
			_, err := deployAndScan(ctx, j, s.lab, s.built, m, cr)
			return err
		default:
			return fmt.Errorf("unknown action %q", action)
		}
	})
}

// prebuild builds the auditor once and stores it on the server, printing the
// stream. It returns a process exit code (0 on success).
func prebuild(s *Server, lab *Lab) int {
	err := runSync("build", func(ctx context.Context, j *Job) error {
		b, err := Build(ctx, j, lab, "")
		if err == nil {
			s.built = b
		}
		return err
	})
	if err != nil {
		return 1
	}
	return 0
}

// --- helpers ----------------------------------------------------------------

func loadLab(path string) (*Lab, int) {
	lab, err := Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, "hint: run 'labctl init …' to scaffold", path)
		return nil, 1
	}
	return lab, 0
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
			// A scanner is never a scan target, and has no baseline to snapshot.
			if m.isScanner() && (action == "scan" || action == "snapshot") {
				continue
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
// returns the job's error. SIGINT cancels the underlying context.
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

func exit(err error) int {
	if err != nil {
		return 1
	}
	return 0
}

// defaultSubnet turns an IPv4 host address into its /24 network string.
func defaultSubnet(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ""
	}
	return fmt.Sprintf("%s.%s.%s.0/24", parts[0], parts[1], parts[2])
}

// nextIP returns the address one above the given IPv4 host (last octet +1),
// used to place the scanner next to the DC by default.
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
