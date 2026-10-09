# lab-creator

A small control panel you run on a **Hyper-V host** to stand up disposable
directory-services test labs and exercise a tool against them — build it from
source, create and provision the lab VMs from ISOs you supply, then deploy the
freshly built binary and scan each target, all from one local web page with a
live log.

It was built to drive the [Directory Auditor](https://github.com/atsvetko/directory-auditor)
labs, and it reads that project's `tools/lab/**` provisioning scripts and
`*-expect.yaml` assertion files from the checkout you point it at.

OS support is **pluggable** — one plugin per directory product, registered in
`internal/labctl/plugins.go`:

| `kind`          | Product                                | Directory tech    | Provider | Guest OS |
|-----------------|----------------------------------------|-------------------|----------|----------|
| `windows-dc`    | Windows Server — Active Directory DC   | AD                | `ad`     | Windows  |
| `alt-domen`     | **Альт Домен** (Basealt ALT)           | Samba AD DC       | `samba`  | Linux    |
| `red-adm`       | **РЕД АДМ** (RED OS)                    | Samba DC          | `samba`  | Linux    |
| `ald-pro`       | **ALD Pro** (Astra Linux)              | FreeIPA           | `freeipa`| Linux    |
| `linux-scanner` | Scanner — audit workstation            | none (not joined) | —        | Linux    |

Each Linux target is provisioned with the **vendor's own** directory packages,
from the distribution's own repositories, at the version that vendor ships — the
lab never installs upstream/generic Samba or FreeIPA, because the point is to
audit what the vendor actually ships:

- **Альт Домен** — `apt-get install task-samba-dc` on ALT ([docs.altlinux.org](https://docs.altlinux.org/ru-RU/cloud/10.0/html/cloud-server/sambadc--chapter.html)).
- **РЕД АДМ** — РЕД АДМ is **not** FreeIPA; its FAQ states FreeIPA is not supported and the Standard edition uses RED OS's base Samba DC ([redadm.red-soft.ru/product/faq](https://redadm.red-soft.ru/product/faq/)). Provisioned with the RED OS vendor Samba-DC packages.
- **ALD Pro** — FreeIPA-based ([wiki.astralinux.ru](https://wiki.astralinux.ru/pages/viewpage.action?pageId=263042308)), installed with the vendor's `aldpro-mp` and promoted with `aldpro-server-install` ([install guide](https://t-wiki.ru/doku.php?id=astralinux:aldproinstall)) — never generic `ipa-server-install`.

The vendor scripts refuse to run on an unexpected OS (checked via `/etc/os-release`)
rather than fall back to an upstream package, and record the installed vendor
version as evidence.

A plugin declares its provider, guest OS, VM firmware, scan transport and how to
provision the domain; `create`, `deploy`, `scan` and `verify` are generic.
Adding another product is adding a `Plugin` (and, if needed, its
`tools/lab/<name>/` scripts in the target repo). Legacy/short names resolve as
aliases (`alt-dc`, `alt-linux`, `samba` → `alt-domen`; `scanner` → `linux-scanner`).

```
Hyper-V host ── labctl (this tool, 127.0.0.1 + one-time token)
  ├─ build:      git fetch + go build  (Windows + Linux binaries)
  ├─ create:     New-VM from your ISO on an isolated switch
  ├─ provision:  promote to DC with the VENDOR's packages + seed weak fixtures
  │              (Windows: PowerShell Direct · Linux: SSH)
  ├─ snapshot:   vuln-baseline checkpoint
  └─ run:        restore → scan (from the non-joined scanner VM) → verify

All scans run from one dedicated Linux scanner VM that is NOT domain-joined; it
binds to each DC over the network with the audit account, exactly as an auditor
would from an admin workstation.
```

## Why

Directory security checks need to be validated against *real* domain
controllers, not only synthetic fixtures. This gives you a repeatable, mostly
unattended way to do that locally: deterministic baselines via checkpoints, one
click to reset-and-scan everything, and a config you edit in the browser.

## Install & run

On the host (Windows with Hyper-V, PowerShell, Go and git on `PATH`):

```powershell
git clone https://github.com/atsvetko/lab-creator
cd lab-creator
copy lab.example.yaml lab.yaml        # then edit, or edit it in the UI
go run .                              # or: go build -o labctl.exe . ; .\labctl.exe
```

It prints a `http://127.0.0.1:<port>/?t=<token>` URL (loopback + one-time token)
and opens it.

## The panel

The UI mirrors Directory Auditor's own look — a pivot across three stages with a
browser-window frame and a live console pinned below.

1. **Setup** — the admin password (provisioning) and the ordinary audit account
   (scans), held in memory for the session only and never written to the config;
   and **Build**, which fetches and builds the chosen git ref (`main` by default,
   or any tag/branch/commit) into `work_dir\bin`, for Windows and Linux.
2. **Lab** — a row per machine with its VM state, a kind pill coloured by guest
   OS, whether a `vuln-baseline` checkpoint exists, the last scan result, and
   per-machine actions (create · provision · snapshot · reset · start · stop ·
   scan), plus **Reset + scan all**. Every scan runs from the `linux-scanner`
   VM against the selected target; the scanner itself is not a scan target.
3. **Config** — add/remove machines and set names, kinds (chosen from the
   registered plugins), domains, IPs, ISOs and sizes; edits save back to
   `lab.yaml`.

The **console** streams the running action's log at all times.

## Config (`lab.yaml`)

```yaml
name: dirauditor-lab
switch: da-lab               # Hyper-V *internal* switch (isolated, no uplink)
repo_dir: C:\src\directory-auditor   # checkout to build and take scripts from
ref: main
work_dir: C:\lab\work
machines:
  - { name: DA-SCANNER, kind: linux-scanner, iso: C:\iso\alt-ws.iso, ip: 10.55.0.9 }   # not joined; runs every scan
  - { name: DA-DC-2022, kind: windows-dc, iso: C:\iso\ws2022.iso, domain: da.test,  ip: 10.55.0.10, forest_mode: Win2016, expect: testdata/lab/windows-expect.yaml }
  - { name: DA-DC-2025, kind: windows-dc, iso: C:\iso\ws2025.iso, domain: da.test,  ip: 10.55.0.11, forest_mode: Win2025, expect: testdata/lab/windows-expect.yaml }
  - { name: DA-ALT,     kind: alt-domen,  iso: C:\iso\alt.iso,    domain: alt.test, ip: 10.55.0.12, expect: testdata/lab/samba-expect.yaml }
  - { name: DA-REDADM,  kind: red-adm,    iso: C:\iso\red.iso,    domain: red.test, ip: 10.55.0.13, expect: testdata/lab/samba-expect.yaml }
  - { name: DA-ALDPRO,  kind: ald-pro,    iso: C:\iso\ald.iso,    domain: ald.test, ip: 10.55.0.14, expect: testdata/lab/freeipa-admin-expect.yaml }
```

- `windows-dc` — promoted and seeded via `tools/lab/windows/*.ps1` over PowerShell Direct.
- `alt-domen`, `red-adm` — Samba DC; provisioned via `tools/lab/samba/provision-vendor.sh` (vendor Samba-DC packages, OS-guarded) + `tools/lab/samba/populate.sh` over SSH.
- `ald-pro` — FreeIPA; provisioned via `tools/lab/aldpro/provision.sh` (`aldpro-mp` + `aldpro-server-install`) + `tools/lab/freeipa/populate.sh` over SSH.
- `linux-scanner` — not provisioned (not joined); the built Linux binary and catalogue are copied to it at scan time.

SSH-based provisioning and scanning assume key-based `root@<ip>` access to the Linux guests.

### Scan transport (from the non-joined scanner)

The scanner has no Kerberos ticket, so the channel depends on the target:

- **Samba / FreeIPA** (`alt-domen`, `red-adm`, `ald-pro`) self-sign LDAPS, so the
  scanner fetches and **pins** the target's LDAPS certificate and scans
  encrypted (`--tls ldaps --pin …`). If the DC isn't serving 636 the scan
  **fails closed** rather than dropping encryption.
- **Windows** (`windows-dc`) — a vanilla DC has no LDAPS and the scanner can't do
  Kerberos sealing without joining, so the scan uses a **lab-only plaintext**
  simple bind (`--tls none --insecure-plaintext`) on the isolated switch. Give
  the DC an LDAPS certificate (e.g. via AD CS) if you want it pinned instead.

## Prerequisites

- Windows host with **Hyper-V** and PowerShell, run as Administrator.
- **Go** (see `go.mod`) and **git** on `PATH`.
- A checkout of the target project in `repo_dir` (its `tools/lab/**` scripts and
  `*-expect.yaml` files are what labctl drives).
- Optional: **oscdimg** (Windows ADK) to auto-answer Windows installs; **ssh/scp**
  for the Linux guests; **openssl** on the scanner VM (to pin LDAPS certs).
- A `linux-scanner` machine — every scan runs from it.
- The install **ISOs** you provide for each OS.

## Security

- Lab lives on an **isolated internal** Hyper-V switch — no route to a real network.
- Provisioning scripts refuse unless `DIRAUDITOR_LAB=1` (labctl sets it).
- Credentials are kept in memory for the session, not in the config file.
- The panel binds `127.0.0.1` with a one-time token.

## Status / limitations

- The Hyper-V / PowerShell / SSH orchestration runs only on a Windows host.
- Windows auto-install needs the ADK's `oscdimg`; without it, install the OS once
  through the Hyper-V console, then use **provision**.
- Linux installs (Альт Домен, РЕД АДМ, ALD Pro, scanner) are interactive in this
  version (their preseed/kickstart varies by build); labctl takes over at
  **provision** over SSH once the guest is up and reachable.
- Scanning from a non-joined box means **on-box Samba `smb.conf` tier-2 checks do
  not run** (they need local file access on the DC). If you want them, run a
  `dirauditor scan --local` on the Samba DC itself as a separate step; the
  network scan from the scanner covers everything else.
- The RED OS vendor Samba-DC package name is resolved on-box from a candidate
  list; confirm it for your RED OS release (the script refuses rather than
  pulling upstream).
- One action runs at a time (the lab VMs are shared hardware).

## Licence

Apache-2.0.
