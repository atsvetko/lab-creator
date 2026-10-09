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
`internal/labctl/plugins.go`. Four ship today:

| `kind`       | Product                               | Provider | Guest OS |
|--------------|---------------------------------------|----------|----------|
| `windows-dc` | Windows Server — Active Directory DC  | `ad`     | Windows  |
| `alt-domen`  | **Альт Домен** (Alt Server, Samba AD) | `samba`  | Linux    |
| `ald-pro`    | **ALD Pro** (Astra, FreeIPA)          | `freeipa`| Linux    |
| `red-adm`    | **РЕД АДМ** (RED OS, FreeIPA)         | `freeipa`| Linux    |

A plugin declares its provider, guest OS, VM firmware and how to provision the
domain; `create`, `deploy`, `scan` and `verify` are generic. Adding another
product is adding a `Plugin` (and, if needed, its `tools/lab/<name>/` scripts in
the target repo). Legacy/short names resolve as aliases (`alt-dc`, `alt-linux`,
`samba` → `alt-domen`; `freeipa` → `ald-pro`).

```
Hyper-V host ── labctl (this tool, 127.0.0.1 + one-time token)
  ├─ build:      git fetch + go build  (Windows + Linux binaries)
  ├─ create:     New-VM from your ISO on an isolated switch
  ├─ provision:  promote to DC + seed weak fixtures (PowerShell Direct / SSH)
  ├─ snapshot:   vuln-baseline checkpoint
  └─ run:        restore → deploy → scan → verify, per machine or all at once
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
   scan), plus **Reset + scan all**.
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
  - { name: DA-DC-2022, kind: windows-dc, iso: C:\iso\ws2022.iso, domain: da.test,  ip: 10.55.0.10, forest_mode: Win2016, expect: testdata/lab/windows-expect.yaml }
  - { name: DA-DC-2025, kind: windows-dc, iso: C:\iso\ws2025.iso, domain: da.test,  ip: 10.55.0.11, forest_mode: Win2025, expect: testdata/lab/windows-expect.yaml }
  - { name: DA-ALT,     kind: alt-domen,  iso: C:\iso\alt.iso,    domain: alt.test, ip: 10.55.0.12, expect: testdata/lab/samba-expect.yaml }
  - { name: DA-ALDPRO,  kind: ald-pro,    iso: C:\iso\ald.iso,    domain: ald.test, ip: 10.55.0.13, expect: testdata/lab/freeipa-admin-expect.yaml }
  - { name: DA-REDADM,  kind: red-adm,    iso: C:\iso\red.iso,    domain: red.test, ip: 10.55.0.14, expect: testdata/lab/freeipa-admin-expect.yaml }
```

- `windows-dc` — promoted and seeded via `tools/lab/windows/*.ps1` over PowerShell Direct; scanned over the network.
- `alt-domen` — a Samba AD DC (**Альт Домен**); provisioned via `tools/lab/samba/*.sh` over SSH and scanned on-box (`--local`, so the `smb.conf` checks apply).
- `ald-pro`, `red-adm` — FreeIPA-based (**ALD Pro**, **РЕД АДМ**); the appliance installs its own IPA domain during OS setup, then `tools/lab/freeipa/*.sh` seeds the weak fixtures over SSH and the scan runs on-box.

All of SSH-based provisioning assumes key-based `root@<ip>` access to the Linux guests.

## Prerequisites

- Windows host with **Hyper-V** and PowerShell, run as Administrator.
- **Go** (see `go.mod`) and **git** on `PATH`.
- A checkout of the target project in `repo_dir` (its `tools/lab/**` scripts and
  `*-expect.yaml` files are what labctl drives).
- Optional: **oscdimg** (Windows ADK) to auto-answer Windows installs; **ssh/scp**
  for the Linux targets (Альт Домен, ALD Pro, РЕД АДМ).
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
- Linux installs (Альт Домен, ALD Pro, РЕД АДМ) are interactive in this version
  (their preseed/kickstart varies by build); labctl takes over at **provision**
  over SSH once the guest is up and reachable. The FreeIPA appliances install
  their own domain during OS setup, so for them **provision** only seeds fixtures.
- One action runs at a time (the lab VMs are shared hardware).

## Licence

Apache-2.0.
