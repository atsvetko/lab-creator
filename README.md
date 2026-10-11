# lab-creator

Builds **directory test labs on demand** on a Hyper-V host. You say which
directories you want — Windows Active Directory, **Альт Домен**, **РЕД АДМ**,
**ALD Pro** — and `labctl` creates the VMs from ISOs you supply, promotes them to
domain controllers, seeds deliberately weak objects, checkpoints a baseline, and
describes the result in `lab-info.json` so any testing tool can use the lab.

One binary. The provisioning scripts are embedded; the host needs no Go, git,
Python or source checkout.

```
Hyper-V host ── labctl (web UI on 127.0.0.1 + one-time token, or CLI)
  ├─ setup:      checklist — host, rights, isolated switch, config, ISOs, credentials
  ├─ create:     New-VM from your ISO on an internal switch (created if missing)
  ├─ provision:  promote to DC with the VENDOR's packages + seed weak objects
  │              (Windows: PowerShell Direct · Linux: SSH)
  ├─ snapshot:   baseline checkpoint
  ├─ reset:      back to the baseline in seconds
  └─ lab-info:   lab-info.json — directories, endpoints, accounts (no passwords)
```

Design and roadmap: [docs/ARCHITECTURE-v2.md](docs/ARCHITECTURE-v2.md).

## Supported guests

| `kind`          | Product                                | Directory      | Guest OS |
|-----------------|----------------------------------------|----------------|----------|
| `windows-dc`    | Windows Server — Active Directory DC   | `ad`           | Windows  |
| `alt-domen`     | **Альт Домен** (Basealt ALT)           | `samba`        | Linux    |
| `red-adm`       | **РЕД АДМ** (RED OS)                   | `samba`        | Linux    |
| `ald-pro`       | **ALD Pro** (Astra Linux)              | `freeipa`      | Linux    |
| `linux-scanner` | Linux workstation for testing tools    | none (not joined) | Linux |

Each Linux target is provisioned with the **vendor's own** directory packages,
from the distribution's own repositories — never upstream/generic Samba or
FreeIPA, because the point is to test what the vendor actually ships:

- **Альт Домен** — `apt-get install task-samba-dc` on ALT ([docs.altlinux.org](https://docs.altlinux.org/ru-RU/cloud/10.0/html/cloud-server/sambadc--chapter.html)).
- **РЕД АДМ** — РЕД АДМ is **not** FreeIPA; its FAQ states FreeIPA is not supported and the Standard edition uses RED OS's base Samba DC ([redadm.red-soft.ru/product/faq](https://redadm.red-soft.ru/product/faq/)). Provisioned with the RED OS vendor Samba-DC packages.
- **ALD Pro** — FreeIPA-based ([wiki.astralinux.ru](https://wiki.astralinux.ru/pages/viewpage.action?pageId=263042308)), installed with the vendor's `aldpro-mp` and promoted with `aldpro-server-install` ([install guide](https://t-wiki.ru/doku.php?id=astralinux:aldproinstall)) — never generic `ipa-server-install`.

## Install & run

On the Hyper-V host (Windows 10/11 Pro or Enterprise, or Windows Server, with Hyper-V enabled):

```powershell
go build -o labctl.exe .        # until signed releases are published
.\labctl.exe                    # opens the web UI
```

Run it as administrator the first time (it creates the internal switch and the
host's address on it); after that, membership in **Hyper-V Administrators** is
enough. `labctl doctor` prints the same checklist as the UI.

## The panel

1. **Setup** — a checklist that must be green before any VM is touched (Windows
   host, Hyper-V, rights, isolated switch, saved and valid config, at least one
   directory server, ISOs, credentials, OpenSSH client for Linux guests), plus
   the lab credentials. Credentials live in memory for the session only.
2. **Config** — add or remove machines; set kinds, domains, IPs, ISOs and sizes.
   Saving writes `lab.yaml`.
3. **Lab** — a row per machine with its VM state and baseline, and the actions
   that are valid right now (others are disabled with the reason as a tooltip),
   plus **Reset all** and **Download lab-info.json**.

The console streams the running action's log.

## Command line (and CI)

```powershell
$env:LAB_ADMIN_PASSWORD = Read-Host -AsSecureString | ConvertFrom-SecureString -AsPlainText
$env:LAB_AUDIT_PASSWORD = Read-Host -AsSecureString | ConvertFrom-SecureString -AsPlainText
.\labctl.exe init --name W2025 --iso D:\iso\WinServer2025_Eval.iso --domain corp.test --ip 10.10.30.10
.\labctl.exe doctor
.\labctl.exe up                 # create + provision + snapshot every machine
.\labctl.exe reset --all        # back to the baseline
.\labctl.exe info --out lab-info.json
```

`labctl help` lists every command. Exit codes: `0` ok · `1` action failed ·
`2` usage or invalid lab file · `3` Setup not ready · `5` provisioning failed
during `up`.

The lab file defaults to `lab.yaml` in the current folder, else next to
`labctl.exe`. A relative `work_dir` resolves next to the lab file.

## Config (`lab.yaml`)

See [lab.example.yaml](lab.example.yaml). In short:

```yaml
name: ru-mixed
switch: lc-lab           # internal switch; created on the first create
subnet: 10.10.30.0/24    # .1 is the host on the lab switch
work_dir: work
ssh_user: altadmin       # Linux login; vendor DCs disable root SSH
machines:
  - { name: W2025,  kind: windows-dc, iso: C:\iso\ws2025.iso, domain: corp.test, ip: 10.10.30.10, forest_mode: Win2025 }
  - { name: ALT,    kind: alt-domen,  iso: C:\iso\alt.iso,    domain: alt.test,  ip: 10.10.30.12 }
  - { name: ALDPRO, kind: ald-pro,    iso: C:\iso\ald.iso,    domain: ald.test,  ip: 10.10.30.14 }
```

Lab files from the Directory Auditor era (`repo_dir`, `ref`, `expect`) still
load; those fields are ignored, reported as a note, and removed on the next save.

## Using the lab from a testing tool

`lab-info.json` (schema `lab-creator/lab-info/v1`) lists each directory with its
type, domain, realm, NetBIOS name, base DN, DC address, LDAP/LDAPS/Kerberos
endpoints and account principals. Passwords are never written to it; the tool
gets them from the same `LAB_ADMIN_PASSWORD` / `LAB_AUDIT_PASSWORD` environment.

Example: point Directory Auditor at each directory in the file, after
`labctl reset --all`, from a CI step or the scanner VM.

## What provisioning does

The embedded scripts live in [`content/`](content/) and refuse to run unless
`LABCTL_LAB=1` (labctl sets it). labctl passes parameters as `LAB_*` variables,
each with one meaning: `LAB_DNS_DOMAIN`, `LAB_REALM`, `LAB_NETBIOS`, `LAB_BASEDN`,
`LAB_HOST`, `LAB_IP`, `LAB_PREFIX`, `LAB_ADMIN_PASSWORD`, `LAB_AUDIT_USER`,
`LAB_AUDIT_PASSWORD`, `LAB_USER_PASSWORD`. For Linux guests they are exported at
the top of the script piped over SSH, so secrets never appear on a command line.

## Security

- Labs are **deliberately weak**. They live on an internal or private Hyper-V
  switch; labctl refuses an external switch.
- Provisioning scripts refuse to run unless `LABCTL_LAB=1`.
- Credentials are kept in memory (UI) or the environment (CLI), never in the
  lab file or `lab-info.json`.
- The panel binds `127.0.0.1` with a one-time token.

## Limitations (now)

- Hyper-V only; the driver interface for VirtualBox, VMware and Proxmox is on
  the roadmap.
- Windows unattended install needs the Windows ADK's `oscdimg`; without it,
  install the OS through the Hyper-V console, then **provision**.
- Linux installs (Альт Домен, РЕД АДМ, ALD Pro, scanner) are interactive for
  now; labctl takes over at **provision** over SSH. The SSH login must be able
  to `sudo` without a password prompt.
- One action runs at a time per labctl instance.

## Licence

Apache-2.0.
