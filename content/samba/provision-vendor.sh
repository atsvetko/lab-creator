#!/usr/bin/env bash
# Provision a throw-away Samba AD DC on a *vendor* Linux guest — Альт Домен
# (Basealt ALT) or РЕД АДМ / РЕД ОС (RED Soft) — using the VENDOR's own Samba
# packages from the distribution's own repositories, at the version that vendor
# ships and supports. It never adds an upstream/third-party repository and never
# builds Samba from source: the whole point of the lab is to audit what the
# vendor actually ships.
#
# This is the VM-lab counterpart to ../samba/provision.sh (which installs a
# generic distro Samba for CI). labctl pipes this over SSH to the guest.
#
# LAB ONLY: it installs packages and overwrites /etc/samba/smb.conf and
# /var/lib/samba. Refuses unless LABCTL_LAB=1. Idempotent: an existing lab
# domain is reused.
#
# Usage: ssh root@dc  'LABCTL_LAB=1 LAB_REALM=ALT.TEST ... bash -s' < provision-vendor.sh [--dry-run]
# Env:   LAB_REALM (ALT.TEST) LAB_NETBIOS (ALT) LAB_HOST (dc1) LAB_IP (the DC's own IP)
#        LAB_ADMIN_PASSWORD  SAMBA_PYTHON (python3)
set -euo pipefail

[[ "${LABCTL_LAB:-}" == "1" ]] || { echo "refusing: set LABCTL_LAB=1 (this script installs packages and replaces the Samba configuration on this machine)"; exit 2; }
DRY=0; [[ "${1:-}" == "--dry-run" ]] && DRY=1
[[ $EUID -eq 0 || $DRY -eq 1 ]] || { echo "run as root"; exit 2; }

REALM=${LAB_REALM:-ALT.TEST}
DOMAIN=${LAB_NETBIOS:-${REALM%%.*}}
HOST=${LAB_HOST:-dc1}
FQDN="$HOST.$(echo "$REALM" | tr 'A-Z' 'a-z')"
HOSTIP=${LAB_IP:-127.0.0.1}
ADMINPW=${LAB_ADMIN_PASSWORD:-'Lab-Admin-Pass-2026!'}
PY=${SAMBA_PYTHON:-python3}
LOG=${LAB_LOG:-/var/log/samba-lab.log}

run() { if (( DRY )); then echo "+ $*"; else "$@"; fi; }

# --- identify the vendor OS -------------------------------------------------
# We only ever install the Samba-DC package set the *vendor* ships. Refuse on an
# OS we don't recognise, rather than silently pulling a generic/upstream Samba.
OS_ID=""; OS_NAME=""
if [[ -r /etc/os-release ]]; then
  # shellcheck disable=SC1091
  . /etc/os-release; OS_ID=${ID:-}; OS_NAME=${PRETTY_NAME:-$NAME}
fi
echo "lab: guest = ${OS_NAME:-unknown} (ID=${OS_ID:-?})"

# --- install the vendor's Samba-DC package set ------------------------------
# ALT (Basealt / Альт Домен): apt-rpm, metapackage task-samba-dc.
#   docs.altlinux.org — «Samba 4 в роли контроллера домена»: apt-get install task-samba-dc
# RED OS (РЕД АДМ Standard uses the base Samba DC): dnf, resolve the shipped
#   package name from a vendor candidate list (RED OS has carried both a
#   task-samba-dc metapackage and a samba-dc package across releases); we use
#   whichever the vendor repo actually provides and never fall back to upstream.
install_vendor_samba() {
  case "$OS_ID" in
    altlinux)
      echo "lab: installing ALT Samba-DC (task-samba-dc) from the ALT repositories"
      run apt-get update
      run apt-get install -y task-samba-dc
      ;;
    redos)
      echo "lab: installing RED OS Samba-DC from the RED OS repositories"
      local pkg=""
      for cand in task-samba-dc samba-dc samba-ad-dc; do
        if dnf -q info "$cand" >/dev/null 2>&1; then pkg="$cand"; break; fi
      done
      [[ -n "$pkg" ]] || { echo "lab: could not find a vendor Samba-DC package in the RED OS repos (tried task-samba-dc, samba-dc, samba-ad-dc). Confirm the package name for this RED OS release and re-run; this script will not install upstream Samba."; exit 3; }
      echo "lab: RED OS Samba-DC package = $pkg"
      run dnf install -y "$pkg"
      ;;
    astra|astralinux)
      echo "lab: Astra guest detected — use the ALD Pro provisioner (FreeIPA), not this Samba one."; exit 3
      ;;
    *)
      echo "lab: unrecognised vendor OS '${OS_ID:-?}'. This provisioner only installs vendor Samba-DC packages for ALT (Альт Домен) or RED OS (РЕД АДМ). Refusing rather than pulling a generic/upstream Samba."; exit 3
      ;;
  esac
}
(( DRY )) || install_vendor_samba

# --- evidence: which Samba the vendor shipped -------------------------------
if (( ! DRY )); then
  ver="$(samba --version 2>/dev/null || true)"
  case "$OS_ID" in
    altlinux) pkgv="$(rpm -q samba-dc 2>/dev/null || rpm -q samba 2>/dev/null || echo '?')" ;;
    redos)    pkgv="$(rpm -q samba-dc 2>/dev/null || rpm -q samba 2>/dev/null || echo '?')" ;;
    *)        pkgv="?" ;;
  esac
  echo "lab: vendor Samba = ${ver:-?}  (package: $pkgv)"
fi

ST="$PY $(command -v samba-tool 2>/dev/null || echo /usr/bin/samba-tool)"
echo "lab: $FQDN ($DOMAIN / $REALM) on ${HOSTIP}"

# 1. Name resolution for the DC itself (Kerberos needs the FQDN).
if ! grep -q "$FQDN" /etc/hosts; then
  run sh -c "echo '$HOSTIP $FQDN $HOST' >> /etc/hosts"
fi

# 1b. An AD DC runs its own smbd/KDC/LDAP; stop and mask anything that conflicts.
if [[ -d /run/systemd/system ]] && command -v systemctl >/dev/null; then
  for u in smb nmb smbd nmbd winbind krb5kdc slapd bind named samba-ad-dc; do
    run systemctl stop "$u" 2>/dev/null || true
    run systemctl disable "$u" 2>/dev/null || true
    run systemctl mask "$u" 2>/dev/null || true
  done
fi
if (( ! DRY )); then
  pkill -x smbd 2>/dev/null || true; pkill -x nmbd 2>/dev/null || true; pkill -x winbindd 2>/dev/null || true
  sleep 1
fi

# 2. Provision once, with the vendor's samba-tool.
if [[ -f /var/lib/samba/private/sam.ldb ]] && (( ! DRY )); then
  echo "lab: existing domain found, keeping it"
else
  run rm -f /etc/samba/smb.conf
  run rm -rf /var/lib/samba/private /var/lib/samba/sysvol
  run $ST domain provision --realm="$REALM" --domain="$DOMAIN" --server-role=dc \
      --dns-backend=SAMBA_INTERNAL --adminpass="$ADMINPW" --host-name="$HOST" \
      --host-ip="$HOSTIP" --use-rfc2307 --option="dns forwarder = 127.0.0.53"
fi

# 3. Deliberately weak settings (each one is a known finding). LAB ONLY.
if (( ! DRY )) && ! grep -q "lab weaknesses" /etc/samba/smb.conf; then
  "$PY" - <<'EOF'
p = '/etc/samba/smb.conf'
s = open(p).read()
weak = """\t# --- lab weaknesses (deliberate; lab only) ---
\tserver schannel = auto
\treject md5 clients = no
\tntlm auth = ntlmv1-permitted
\tallow dns updates = nonsecure
\tserver min protocol = NT1
\tdsdb:schema update allowed = yes
\told password allowed period = 1440
\tmap to guest = Bad User
\tkdc:user ticket lifetime = 24
\tkerberos encryption types = legacy
"""
head, sep, rest = s.partition('\n[sysvol]')
s = head.rstrip('\n') + '\n' + weak + sep + rest
s += "\n[public]\n\tpath = /srv/public\n\tread only = No\n\tguest ok = Yes\n"
open(p, 'w').write(s)
EOF
  run mkdir -p /srv/public
fi
run sh -c "testparm -s --suppress-prompt /etc/samba/smb.conf >/dev/null"

# 4. Kerberos client config for the lab realm.
run cp /var/lib/samba/private/krb5.conf /etc/krb5.conf

# 5. Start samba (prefer the vendor's systemd unit; fall back to foreground).
if (( ! DRY )); then
  run $PY /usr/sbin/samba_spnupdate 2>/dev/null || true
  started=0
  if [[ -d /run/systemd/system ]] && command -v systemctl >/dev/null; then
    for u in samba samba-ad-dc; do
      if systemctl list-unit-files 2>/dev/null | grep -q "^$u"; then
        systemctl unmask "$u" 2>/dev/null || true
        if systemctl enable --now "$u" 2>/dev/null; then started=1; break; fi
      fi
    done
  fi
  if (( ! started )) && ! pgrep -x samba >/dev/null; then
    nohup samba -i -s /etc/samba/smb.conf >"$LOG" 2>&1 & disown
  fi
  for i in $(seq 1 30); do
    if ldapsearch -x -H ldap://127.0.0.1 -s base -b "" vendorVersion 2>/dev/null | grep -q vendorVersion; then
      echo "lab: LDAP up after ${i}s"; break
    fi
    sleep 1
    [[ $i -eq 30 ]] && { echo "lab: samba did not come up; log:"; tail -20 "$LOG" 2>/dev/null; tail -20 /var/log/samba/log.smbd 2>/dev/null || true; exit 1; }
  done
  ldapsearch -x -H ldap://127.0.0.1 -s base -b "" vendorVersion defaultNamingContext domainFunctionality 2>/dev/null | grep -E "^(vendorVersion|defaultNamingContext|domainFunctionality)" || true
  if [[ -f /var/lib/samba/private/tls/cert.pem ]]; then
    echo "lab: LDAPS pin $(openssl x509 -in /var/lib/samba/private/tls/cert.pem -noout -fingerprint -sha256 | cut -d= -f2 | tr -d ':' | tr 'A-F' 'a-f')"
  fi
fi
echo "lab: vendor Samba DC provisioning done"
