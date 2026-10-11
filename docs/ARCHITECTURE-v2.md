# lab-creator — architecture v2

> Changelog: v2 (2026-10-11) — decisions from v1 review: Hyper-V only for now (driver interface kept); fresh ISO install for every lab (no base-image cache in v1); three deployment modes — laptops, a shared Hyper-V server, CI — which adds multi-lab hosts, namespacing, admission control and a CI contract. v1 (2026-10-11) — first cut.

## 1. Why

A lab is a **test directory on demand**: you declare which directory you want (AD, Samba DC, FreeIPA; Windows Server, Альт Домен, РЕД АДМ, ALD Pro), and lab-creator stands it up in a known state, so any tool — Directory Auditor, DD+DAT, a pentest kit, a CI job — can test against it.

The differentiator is not "VMs from YAML" (GOAD, AutomatedLab and Ludus do that for Microsoft AD). It is **Russian directory products + a declarative directory model + a machine-readable manifest of the seeded weaknesses** that testers assert against.

## 2. Decisions (v1 review)

| # | Question | Decision | Consequence |
|---|---|---|---|
| D1 | Hypervisors | **Hyper-V now.** Others later. | Only the Hyper-V driver is built; the `Driver` interface stays so VirtualBox/VMware/Proxmox are additions, not rewrites. |
| D2 | Images | **Fresh ISO install for every lab.** | Simple and always clean; a lab takes tens of minutes to build. Reset is still fast (checkpoint restore). A base-image cache is a later optimisation behind the same interface. |
| D3 | Users | **Laptops, a shared server and CI.** | One host can run several labs for several people at once: namespacing, subnet allocation, resource admission, owners, expiry, and a non-interactive CLI contract. |

## 3. Goals and non-goals

**Goals**
- G1. Deploy directory labs to Hyper-V (Windows 10/11 Pro/Enterprise or Windows Server). Driver interface ready for others.
- G2. One lab model, three front ends: web UI, lab file, CLI flags. All produce the same spec.
- G3. Zero install: one binary with all provisioning content embedded. No Go, git, Python or Ansible on the host; no Directory Auditor checkout.
- G4. Repeatable: every lab is installed fresh from ISO; after provisioning it is checkpointed as `baseline`; `reset` restores it in seconds.
- G5. Idempotent and resumable: `up` twice changes nothing; a failed `up` resumes from the last good step.
- G6. Several labs per host, isolated from each other and from the real network.
- G7. A stable output contract (`lab-info.json`) and a CI-friendly CLI (non-interactive, JSON output, meaningful exit codes, timeouts, guaranteed cleanup).

**Non-goals (now)**
- Running the tests. Testers consume the lab through `lab-info.json` and hooks.
- Hypervisors other than Hyper-V; cloud.
- A multi-user web portal with its own accounts. On a shared server, people use the CLI or the local UI over RDP (see 5.2).
- Redistributing OS images. ISOs are supplied by the user; licences stay theirs.

## 4. Layered architecture

```
 Front ends        Web UI          lab.yaml        CLI flags
                      \               |               /
                       └──────► Spec (lab-creator/v1) ◄┘     validate + defaults
                                      |
 Engine           Host registry ── Planner: spec + state → plan
 (per host)       (labs, owners,          |
                   subnets, RAM)   Executor: plan → actions (resumable, logged, per-lab lock)
                         ┌────────────┼──────────────┐
 Content           Products (unattended   Roles (dc, member,   Scenarios (weak
                   OS install)            scanner)             fixtures + seed manifest)
                         └────────────┼──────────────┘
                                      |
 Transport          WinRM (Windows) · SSH (Linux) · PowerShell Direct (Hyper-V fast path)
                                      |
 Driver             Hyper-V   (VirtualBox · VMware · Proxmox later, same interface)
                                      |
 Outputs        state.json · lab-info.json · lab-secrets.json · event log · hooks
```

### 4.1 Spec (`apiVersion: lab-creator/v1`)

Directory-centric: you declare directories; machines are derived and can be overridden.

```yaml
apiVersion: lab-creator/v1
name: ru-mixed
owner: atsvetko             # default: current user; used on shared hosts
ttl: 8h                     # optional: auto-destroy after this (CI and shared hosts)
network:
  cidr: auto                # auto = allocated from the host pool; or e.g. 10.10.30.0/24
  egress: none              # none | install  (NAT only while installing packages)
directories:
  - name: corp
    type: ad                # ad | samba | freeipa
    product: windows-server-2025
    domain: corp.test
    level: Win2025
    scenarios: [baseline-weak, kerberoastable, shadow-admins]
  - name: alt
    type: samba
    product: alt-domen-11
    domain: alt.test
    scenarios: [baseline-weak]
machines:                   # optional extras or overrides
  - { name: scanner, role: scanner, product: alt-workstation-11 }
isos:                       # product → ISO; may also come from the host config
  windows-server-2025: C:\iso\ws2025-eval.iso
  alt-domen-11:        C:\iso\alt-domen-11.iso
hooks:
  after_up:    []
  after_reset: []
```

CLI shortcut to the same spec: `labctl up --dir ad:windows-server-2025:corp.test --dir samba:alt-domen-11:alt.test`; `labctl spec` prints the spec the flags produced.

### 4.2 Engine

- **Planner** diffs spec, `state.json` and what Hyper-V reports into a plan: network → VMs → OS install → role → scenarios → checkpoint.
- **Executor** runs the plan, records each finished step, resumes after failure. Each step is idempotent and ends with a readiness probe (for example "LDAP on 389 answers and the naming context is readable").
- **Per-lab lock** instead of today's single global job, so labs on one host run in parallel; a host-wide limit caps concurrent OS installs (disk I/O bound).

### 4.3 Driver

Hyper-V only now, behind the interface that later drivers implement:

```go
type Driver interface {
    Name() string
    Detect(ctx) (Info, error)                 // installed, version, admin rights, free RAM/disk
    EnsureNetwork(ctx, Net) (NetRef, error)   // per-lab internal switch + host vNIC
    CreateVM(ctx, VMSpec) (VMRef, error)      // disk, CPU, RAM, ISO + answer media attached
    Start, Stop(ctx, VMRef) error
    Snapshot, Restore(ctx, VMRef, name string) error
    IP(ctx, VMRef) (netip.Addr, error)
    Destroy(ctx, VMRef) error
    DestroyNetwork(ctx, NetRef) error
}
```

Hyper-V implementation: PowerShell cmdlets (as today), Generation 2 VMs, an internal switch per lab named `lc-<lab>`, PowerShell Direct as an optional fast path for Windows guests.

### 4.4 Install: fresh from ISO for every lab

1. labctl generates an **answer-media ISO** in pure Go (no Windows ADK `oscdimg`): `autounattend.xml` for Windows; preseed for Astra/ALD Pro; kickstart for RED OS; the ALT autoinstall profile for Альт.
2. The VM boots from your ISO with the answer media attached, installs unattended, enables WinRM/SSH and reports ready.
3. Role and scenarios are applied over the network; the VM is checkpointed `baseline`.

Where a vendor installer cannot be fully automated, the product is marked `install: manual`: labctl creates the VM, waits for you to finish the installer in the Hyper-V console, then continues. A base-image cache (install once, clone many) can be added later inside `CreateVM` without changing the spec.

### 4.5 Content

Embedded in the binary (`go:embed`) and versioned as one content pack:

- `products/` — unattended-install templates per OS.
- `roles/` — `ad-dc`, `samba-dc` (Альт Домен, РЕД АДМ), `freeipa-server` (ALD Pro), `member`, `scanner`.
- `scenarios/` — weak fixtures, each with a **seed manifest**:

```yaml
scenario: shadow-admins
applies_to: [ad, samba]
seeds:
  - id: SA-001
    object: "CN=svc-helpdesk,OU=Service,..."
    weakness: GenericAll on Domain Admins
    attack: T1098
```

The provisioning scripts move here from the Directory Auditor repo (`tools/lab/**`); the auditor's `*-expect.yaml` files become its own assertions against these seed manifests.

### 4.6 Outputs and consumer contract

- `state.json` — internal: VM ids, checkpoints, content version, step progress.
- `lab-info.json` — versioned public contract: directories, domains, DC hostnames and IPs, LDAP/LDAPS/Kerberos endpoints, CA certificate, accounts by purpose (`admin`, `audit-user`), applied scenarios with seed manifests.
- `lab-secrets.json` — generated throwaway passwords, ACL'd to the owner; `labctl creds` prints them; CI reads them from the file, never from logs.
- Hooks — `after_up` / `after_reset` run with `LAB_INFO=<path>`; `labctl exec <machine> -- <cmd>` runs anything inside the lab.

## 5. Deployment modes

### 5.1 Laptop

One user, typically one lab at a time. Double-click `labctl.exe` → web UI on `127.0.0.1` with a one-time token. `lab.yaml` and the work directory resolve **next to the binary**, so a double-click always finds them. Setup step (doctor checklist) gates every action.

### 5.2 Shared Hyper-V server

Several colleagues, several labs at once on one Windows Server with Hyper-V.

- **Access (now):** people sign in to the server (RDP, or SSH with OpenSSH Server) and use the CLI or the local UI. No network-exposed UI, so no new authentication surface. A remote UI with Windows authentication and TLS is a later step.
- **Host config** `C:\ProgramData\lab-creator\host.yaml`: ISO library, subnet pool (e.g. `10.200.0.0/16` split into `/24`s), RAM/disk reserve, max concurrent installs, default TTL.
- **Namespacing:** VMs `lc-<lab>-<machine>`, switch `lc-<lab>`, files under `labs\<lab>\`. Lab names unique per host.
- **Admission control:** `plan` computes RAM/disk need; `up` refuses if the host would drop below the reserve, and says what is using it (`labctl ls --all`).
- **Owners and expiry:** each lab records its owner; `ttl` destroys it automatically; `labctl gc` removes expired labs (run as a scheduled task). Only the owner or an administrator can destroy a lab.
- **Privileges:** VM operations need membership in *Hyper-V Administrators*, not local Administrators. Host networking (an IP on the host vNIC, NAT) needs an administrator; `labctl host init` runs once as administrator and pre-creates the pool, so day-to-day users don't need admin rights. To be validated on the first server run.

### 5.3 CI

A self-hosted runner on the shared server (or a dedicated Hyper-V host), e.g. a GitHub Actions self-hosted runner.

- Non-interactive: `labctl up -f lab.yaml --name ci-$RUN_ID --ttl 3h --wait --timeout 90m --json`; exit codes: 0 ok, 2 invalid spec, 3 host check failed, 4 insufficient resources, 5 provisioning failed, 6 timeout.
- Always clean up: `labctl destroy --name ci-$RUN_ID` in an always-run step; TTL as the safety net.
- Logs: structured JSON events on stdout, human log in a file the job uploads as an artifact; secrets never printed.
- **Warm labs** (because D2 makes each fresh build slow): a long-lived lab built nightly; CI jobs take a per-job lease, `reset` to `baseline`, test, release. Fresh builds run nightly and whenever the content pack changes.

## 6. Build-vs-buy (unchanged from v1)

Recommendation stays **own Go engine**: the single-binary, zero-dependency principle drives adoption, and the expensive part — unattended install and directory setup for Альт Домен, РЕД АДМ, ALD Pro — has to be written in every option. GOAD (GPL-3.0, Vagrant + Ansible + Packer) and AutomatedLab (MIT, Hyper-V and Azure) are sources of ideas, not dependencies; Vagrant and Packer are under the Business Source License since August 2023.

## 7. Delivery plan

| Phase | Scope | Effort |
|---|---|---|
| 1a | Decouple from Directory Auditor: provisioning content embedded; `repo_dir`, build, scan and verify removed; lab file next to the binary; Setup step gates actions | 2–3 days |
| 1b | Spec v1 (directories), planner/executor with `state.json`, resumable steps, `lab-info.json`, `lab-secrets.json`, Driver interface with Hyper-V implementation | 1 week |
| 2 | Shared server: host config, namespacing, subnet pool, admission control, owners, TTL, `gc`, per-lab locks | 1 week |
| 3 | CI contract: `--json`, exit codes, `--wait/--timeout`, warm labs with leases; example GitHub Actions workflow | 3–5 days |
| 4 | Pure-Go answer-media ISO; Windows Server 2022/2025 and Альт Домен fully unattended | 1 week |
| 5 | РЕД АДМ, ALD Pro; scenario seed manifests; trusts | 1–2 weeks |
| Later | VirtualBox / VMware / Proxmox drivers; base-image cache; remote UI with Windows auth | — |

## 8. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Fresh installs make CI slow (tens of minutes per DC) | Warm labs + `reset` for jobs; fresh builds nightly and on content changes |
| Shared host runs out of RAM/disk; one user's lab starves another's | Admission control with host reserve; TTL; `gc`; `ls --all` shows owners |
| Two labs collide on subnet or names | Subnet pool allocator; `lc-<lab>-` prefixes; uniqueness check at `plan` |
| Weak labs reach the real network | Internal switch per lab; `egress: none` after install; refuse external switches; guard variable in every script |
| Vendor installers change answer-file formats | Products pinned by ISO hash; `install: manual` fallback |
| ALD Pro and Linux roles need package repos during install | `egress: install` NAT only during install, then removed; or a local mirror on the server |
| Windows evaluation ISOs expire (180 days) | Irrelevant for fresh labs; `doctor` warns on old ISOs |
| Throwaway credentials leak through CI logs | Secrets only in `lab-secrets.json`; masked in events; never in `lab.yaml` |
| Users need local admin on the shared server | One-time `host init` as admin; day-to-day only *Hyper-V Administrators* (to be validated) |

## 9. Open questions

1. Shared server remote access beyond RDP/SSH — needed soon, or later? (default: later)
2. CI system: GitHub Actions (default), GitLab, or other?
