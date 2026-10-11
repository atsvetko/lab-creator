# lab-creator — architecture v1 (draft for discussion)

> Changelog: v1 (2026-10-11) — first cut. Turns lab-creator from a Directory Auditor test harness into a standalone, hypervisor-agnostic builder of test directories.

## 1. Why (one sentence)

A lab is a **test directory on demand**: you declare which directory you want (AD, Samba DC, FreeIPA; Windows, Альт Домен, РЕД АДМ, ALD Pro), and lab-creator stands it up on whatever hypervisor you have, in a known state, so any tool (Directory Auditor, DD+DAT, a pentest kit, a CI job) can test against it.

The differentiator is not "VMs from YAML" — GOAD, AutomatedLab and Ludus already do that for Microsoft AD. It is **Russian directory products + a declarative directory model + a machine-readable description of the seeded weaknesses** that testers can assert against.

## 2. Goals and non-goals

**Goals**
- G1. Deploy to the hypervisor that is available: Hyper-V, VirtualBox, VMware Workstation; Proxmox later; cloud as a later option.
- G2. One lab model, three front ends: web UI, lab file, command-line flags. All three produce the same spec.
- G3. Zero install: one signed binary, all provisioning content embedded. No Go, git, Python, Ruby or Ansible on the host.
- G4. Fast and repeatable: build a base image once per OS × hypervisor, then clone; reset to baseline in seconds via snapshots.
- G5. Idempotent: `up` twice changes nothing; `plan` shows what would change.
- G6. A stable output contract (`lab-info.json`) that test tools consume.

**Non-goals (v1)**
- Running the tests. Testing tools consume the lab; lab-creator only offers hooks.
- Production-grade directories. Labs are deliberately weak and live on isolated networks.
- Redistributing OS images. You supply ISOs; licences stay yours.

## 3. Layered architecture

```
 Front ends        Web UI          lab.yaml        CLI flags
                      \               |               /
                       └──────► Spec (lab/v1) ◄──────┘     validate + defaults
                                      |
 Engine                     Planner: spec + state → plan    (diff, like `terraform plan`)
                                      |
                            Executor: plan → actions        (ordered, resumable, logged)
                         ┌────────────┼──────────────┐
 Content           Roles (dc, member,   Scenarios (weak       Images (base OS
                   scanner)             fixture packs)        templates)
                         └────────────┼──────────────┘
                                      |
 Transport          WinRM (Windows) · SSH (Linux) · console fallback
                                      |
 Drivers         Hyper-V │ VirtualBox │ VMware Workstation │ Proxmox │ (cloud)
                                      |
 Outputs        state.json · lab-info.json · secrets file · event log · hooks
```

### 3.1 Spec (`apiVersion: lab-creator/v1`)

The spec is **directory-centric**: you declare directories; machines are derived, but can be overridden.

```yaml
apiVersion: lab-creator/v1
name: ru-mixed
hypervisor: auto            # auto | hyperv | virtualbox | vmware | proxmox
network:
  cidr: 10.10.30.0/24
  egress: none              # none | nat  (nat only while installing packages)
directories:
  - name: corp
    type: ad                # ad | samba | freeipa
    product: windows-server-2025
    domain: corp.test
    level: Win2025
    dcs: 1
    scenarios: [baseline-weak, kerberoastable, shadow-admins]
  - name: alt
    type: samba
    product: alt-domen-11
    domain: alt.test
    scenarios: [baseline-weak]
  - name: ipa
    type: freeipa
    product: ald-pro-2.4
    domain: ipa.test
trusts:
  - { from: corp, to: alt, type: forest, direction: two-way }   # later
machines:                     # optional: extra or overridden machines
  - { name: scanner, role: scanner, product: alt-workstation-11 }
images:
  windows-server-2025: { iso: C:\iso\ws2025-eval.iso }
  alt-domen-11:        { iso: C:\iso\alt-domen-11.iso }
  ald-pro-2.4:         { iso: C:\iso\astra-1.8.iso }
hooks:
  after_up:    []          # commands run on the host or a named machine
  after_reset: []
```

CLI flags are a shortcut to the same spec:
`labctl up --dir ad:windows-server-2025:corp.test --dir samba:alt-domen-11:alt.test --hypervisor virtualbox`
`labctl spec` prints the spec the flags produced, so a one-off becomes a saved file.

### 3.2 Engine

- **Planner** compares the spec with `state.json` and with what the driver reports, and produces a plan: create network, build image, clone VM, install role, apply scenario, snapshot.
- **Executor** runs the plan step by step, records each completed step in `state.json`, and resumes from the last good step after a failure. Every step is idempotent and has a readiness probe (for example, "LDAP answers on 389 and the domain naming context is readable").
- One lab runs at a time per host by default (VMs share RAM and disk).

### 3.3 Drivers (hypervisor abstraction)

A small Go interface with capability flags. Drivers shell out to the vendor's own tool, which exists whenever the hypervisor is installed, so lab-creator adds no dependencies.

```go
type Driver interface {
    Name() string
    Detect(ctx) (Info, error)                 // installed? version? usable? (e.g. Hyper-V needs admin)
    Caps() Caps                               // LinkedClone, Snapshot, HostOnlyNet, NestedVirt, ...
    EnsureNetwork(ctx, Net) (NetRef, error)   // isolated network with a host adapter
    BuildImage(ctx, ImageSpec) (ImageRef, error) // install OS from ISO + answer media, generalise, keep as template
    Clone(ctx, ImageRef, VMSpec) (VMRef, error)
    Start, Stop(ctx, VMRef) error
    Snapshot, Restore(ctx, VMRef, name string) error
    IP(ctx, VMRef) (netip.Addr, error)
    Destroy(ctx, VMRef) error
}
```

| Driver | Control surface | Isolated net with host access | Clone | Notes |
|---|---|---|---|---|
| Hyper-V | PowerShell cmdlets | Internal switch | Differencing VHDX | Exists today; port to the interface |
| VirtualBox | `VBoxManage` | Host-only network | Linked clone | Cross-platform host (Windows, Linux) |
| VMware Workstation | `vmrun` / `vmcli` + `.vmx` templates | Host-only vmnet | Linked clone | Free for commercial use since Nov 2024 |
| Proxmox | REST API | Bridge / SDN VNet | Linked clone from template | For a shared team lab server |
| Cloud (later) | Provider API | VNet/VPC | Marketplace image | Windows eval ISOs do not apply; use marketplace images |

`hypervisor: auto` picks the first usable driver from `labctl doctor` and says which one it picked.

### 3.4 Images: build once, clone many

Installing every VM from ISO takes 20–60 minutes and fails in different ways on each hypervisor. Instead:

1. **Base image** per product × hypervisor, built once from your ISO: boot with a generated **answer-media ISO** (autounattend.xml for Windows; preseed for Astra/ALD Pro; kickstart for RED OS; ALT autoinstall profile for Альт), install guest tools, enable WinRM/SSH, generalise (sysprep / cloud-init clean), keep as template.
2. **Lab VMs** are linked clones of the base image: minutes instead of an hour, and a fraction of the disk.
3. Answer-media ISOs are written by labctl itself in pure Go, so the Windows ADK `oscdimg` dependency goes away.

Images are cached by `product + ISO SHA-256 + content version`, so a new ISO or a fixed install script rebuilds only what changed.

### 3.5 Content: roles, scenarios, products

Embedded in the binary (`go:embed`), versioned as one **content pack**:

- `products/` — how to install each OS unattended (answer-file templates, guest-tools step).
- `roles/` — what a machine becomes: `ad-dc`, `samba-dc` (Альт Домен, РЕД АДМ), `freeipa-server` (ALD Pro, upstream FreeIPA), `member`, `scanner`.
- `scenarios/` — fixture packs that make the directory interesting: weak passwords, Kerberoastable SPNs, shadow admins, dangerous ACLs, legacy protocols. Each scenario ships a **manifest of what it seeded**:

```yaml
scenario: shadow-admins
applies_to: [ad, samba]
seeds:
  - id: SA-001
    object: "CN=svc-helpdesk,OU=Service,..."
    weakness: GenericAll on Domain Admins
    attack: T1098
```

A tester asserts its findings against `seeds`; that replaces today's `*-expect.yaml` files that live in the Directory Auditor repo.

### 3.6 Transport

After the base image exists, every role and scenario runs over the network from the host: **WinRM** to Windows, **SSH** to Linux, on the isolated network through the host adapter. This works identically on every hypervisor. Hyper-V PowerShell Direct stays as an optional fast path, not a requirement.

### 3.7 Outputs and the consumer contract

- `state.json` — what exists (VM ids, snapshots, content version). Internal.
- `lab-info.json` — versioned public contract: directories, domains, DC hostnames and IPs, LDAP/LDAPS/Kerberos endpoints, CA certificate, accounts by purpose (`admin`, `audit-user`), scenarios applied with their seed manifests.
- `lab-secrets.json` — generated throwaway passwords, kept separate, file ACL restricted to the current user; `labctl creds` prints them.
- Hooks — `after_up` / `after_reset` commands get `LAB_INFO=<path>`. `labctl exec <machine> -- <command>` runs anything inside the lab.

This is how Directory Auditor plugs in: its CI runs `labctl up -f ru-mixed.yaml && labctl reset && dirauditor scan --lab-info lab-info.json`.

### 3.8 Front ends

- **CLI** — `doctor`, `plan`, `up`, `reset`, `down`, `destroy`, `status`, `exec`, `creds`, `images build|list|prune`, `spec`.
- **Web UI** — local (`127.0.0.1` + one-time token). Steps: 0 Setup (doctor checklist; all actions disabled until green) → 1 Design (directories and scenarios) → 2 Plan (what will happen, time and disk estimate) → 3 Deploy (live log) → 4 Use (lab-info, credentials, reset, destroy).
- Both call the same engine through one local API; the UI never has logic the CLI lacks.
- `lab.yaml` and work directories resolve next to the binary unless given explicitly, so a double-click finds them.

## 4. Build-vs-buy decision

| Option | Install for the user | Hypervisors | Russian products | Effort | Licence risk |
|---|---|---|---|---|---|
| **A. Own Go engine + drivers (recommended)** | One binary | All four, added one by one | First-class | Highest | None (Apache-2.0) |
| B. Wrap Vagrant + Ansible (GOAD model) | Vagrant + Ruby + Python/Ansible; Ansible control node on Windows needs WSL | VirtualBox, Hyper-V, Docker built in; VMware via plugin | Must write all of it anyway | Medium | Vagrant is BSL 1.1 since Aug 2023 |
| C. Packer + OpenTofu | Two toolchains + providers | Broad | Must write all of it anyway | Medium-high | Packer is BSL; OpenTofu MPL |
| D. Proxmox/Ludus only | A dedicated server | Proxmox only | Must write all of it anyway | Low | Check Ludus licence |
| E. Do nothing (Hyper-V only, as today) | Go + git + clone | Hyper-V | Partial | None | None |

**Recommendation: A**, reusing GOAD/AutomatedLab ideas (lab catalogue, scenario packs) but not their runtimes. Reason: the single-binary, zero-dependency principle is what makes admins try the tool, and the expensive part — unattended install and directory setup for Альт Домен, РЕД АДМ and ALD Pro — has to be written in every option.

**What would change this:** if the main users turn out to be a team with a shared server, D (Proxmox driver first) wins; if Windows-AD-only labs are enough, contribute to GOAD instead of building.

## 5. Delivery plan (rough)

| Phase | Scope | Effort |
|---|---|---|
| 1 | Spec v1, planner/executor, state, Hyper-V driver ported, content embedded, Setup gating, lab-info contract. Directory Auditor coupling removed. | 1.5–2 weeks |
| 2 | Base images + linked clones on Hyper-V; Windows Server 2022/2025 and Альт Домен end to end | 1 week |
| 3 | VirtualBox driver | 3–5 days |
| 4 | VMware Workstation driver | 3–5 days |
| 5 | РЕД АДМ, ALD Pro products; scenario manifests; trusts | 1–2 weeks |
| 6 | Proxmox driver; signed releases | 1 week |

Each product × hypervisor pair needs one real run; 4 products × 3 desktop hypervisors = 12 combinations. That validation, not the code, is the long pole.

## 6. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Hyper-V and VirtualBox/VMware on one Windows host: with Hyper-V enabled, the others run on top of the Windows hypervisor, slower and with quirks | `doctor` detects it and warns; recommend one hypervisor per host |
| Vendor installers (Альт, РЕД, Astra) change answer-file formats between builds | Products pinned by ISO hash; "install manually, then continue" fallback per product |
| ALD Pro and some Linux roles need package repositories during install | `egress: nat` only during build, then cut; or a local repo mirror |
| Windows evaluation ISOs expire after 180 days | Base image records build date; `doctor` warns; rebuild is one command |
| Weak labs leak onto a real network | Isolated network by default; `egress: none` after build; guard env var on every script; refuse bridged adapters |
| Throwaway credentials reused elsewhere | Generated per lab, separate secrets file, never in `lab.yaml` |
| Scope creep into a general lab platform | Non-goals above; directories first, everything else via hooks |

## 7. Open questions

1. Hypervisor order after Hyper-V: VirtualBox → VMware → Proxmox (default), or Proxmox earlier for a shared team server?
2. Base images + linked clones (default), or fresh ISO install per lab (simpler, much slower)?
3. Primary user: you and colleagues on laptops (default), a shared lab server, or CI?
