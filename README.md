# lab-creator

A small control panel you run on a **Hyper-V host** to stand up disposable
directory-services test labs and exercise a tool against them — build it from
source, create and provision the lab VMs from ISOs you supply, then deploy the
freshly built binary and scan each target, all from one local web page with a
live log.

It was built to drive the [Directory Auditor](https://github.com/atsvetko/directory-auditor)
labs (Windows Active Directory DCs and Samba-based **Альт Домен** DCs), and it
reads that project's `tools/lab/**` provisioning scripts and `*-expect.yaml`
assertion files from the checkout you point it at.

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

1. **Credentials** — the admin password (provisioning) and the ordinary audit
   account (scans). Held in memory for the session only; never written to the
   config.
2. **Build the tool** — fetches and builds the chosen git ref (`main` by
   default, or any tag/branch/commit) into `work_dir\bin`, for Windows and Linux.
3. **Lab** — a row per machine with its VM state, whether a `vuln-baseline`
   checkpoint exists, the last scan result, and per-machine actions
   (create · provision · snapshot · reset · start · stop · scan), plus
   **Reset + scan all**. **Edit config** adds/removes machines and sets names,
   kinds, domains, IPs, ISOs and sizes.
4. **Log** — a live stream of the running action.

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
  - { name: DA-ALT,     kind: alt-dc,     iso: C:\iso\alt.iso,    domain: alt.test, ip: 10.55.0.12, expect: testdata/lab/samba-expect.yaml }
```

- `kind: windows-dc` — provisioned via `tools/lab/windows/*.ps1` over PowerShell Direct.
- `kind: alt-dc` — a Samba AD DC (Альт Домен); provisioned via `tools/lab/samba/*.sh` over SSH (key-based `root@<ip>`).

## Prerequisites

- Windows host with **Hyper-V** and PowerShell, run as Administrator.
- **Go** (see `go.mod`) and **git** on `PATH`.
- A checkout of the target project in `repo_dir` (its `tools/lab/**` scripts and
  `*-expect.yaml` files are what labctl drives).
- Optional: **oscdimg** (Windows ADK) to auto-answer Windows installs; **ssh/scp**
  for the Альт Домен target.
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
- Альт Домен installs are interactive in this version (its preseed varies by
  build); labctl takes over at **provision** over SSH.
- One action runs at a time (the lab VMs are shared hardware).

## Licence

Apache-2.0.
