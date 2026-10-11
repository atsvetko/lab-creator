#!/usr/bin/env bash
# Provision a throw-away ALD Pro domain controller on an Astra Linux guest using
# ALD Pro's OWN vendor packages and installer — never a generic/upstream
# FreeIPA. ALD Pro is РусБИТех/ГК «Астра»'s directory service, built on FreeIPA;
# it is installed from the Astra ALD Pro repository (aldpro-mp) and promoted to
# a controller with the vendor's `aldpro-server-install`, not `ipa-server-install`.
#   Sources: dl.astralinux.ru/aldpro (vendor repo); ALD Pro install guides.
#
# This runs the vendor install path only; the deliberately-weak lab fixtures are
# seeded afterwards by ../freeipa/populate.sh (ALD Pro speaks the IPA API).
#
# LAB ONLY: installs packages and stands up a domain. Refuses unless
# LABCTL_LAB=1. Idempotent: an existing controller is kept.
#
# Usage: ssh root@dc 'LABCTL_LAB=1 LAB_DOMAIN=ald.test ... bash -s' < provision.sh [--dry-run]
# Env:   LAB_DNS_DOMAIN (ald.test) LAB_HOST (srv-ald) LAB_IP (the DC's own IP)
#        LAB_ADMIN_PASSWORD
set -euo pipefail

[[ "${LABCTL_LAB:-}" == "1" ]] || { echo "refusing: set LABCTL_LAB=1 (this script installs ALD Pro and stands up a domain on this machine)"; exit 2; }
DRY=0; [[ "${1:-}" == "--dry-run" ]] && DRY=1
[[ $EUID -eq 0 || $DRY -eq 1 ]] || { echo "run as root"; exit 2; }

DOMAIN=${LAB_DNS_DOMAIN:-${LAB_DOMAIN:-ald.test}}
HOST=${LAB_HOST:-srv-ald}
HOSTIP=${LAB_IP:-}
ADMINPW=${LAB_ADMIN_PASSWORD:-'Lab-Admin-Pass-2026!'}

run() { if (( DRY )); then echo "+ $*"; else "$@"; fi; }

# --- require an Astra guest -------------------------------------------------
OS_ID=""; OS_NAME=""
if [[ -r /etc/os-release ]]; then
  # shellcheck disable=SC1091
  . /etc/os-release; OS_ID=${ID:-}; OS_NAME=${PRETTY_NAME:-$NAME}
fi
echo "lab: guest = ${OS_NAME:-unknown} (ID=${OS_ID:-?})"
case "$OS_ID" in
  astra|astralinux) : ;;
  *) echo "lab: ALD Pro installs only on Astra Linux SE; this guest is '${OS_ID:-?}'. Refusing (use the Samba provisioner for ALT/RED OS)."; exit 3 ;;
esac
[[ -n "$HOSTIP" ]] || { echo "lab: set LAB_IP to the controller's own IP (aldpro-server-install needs --ip)"; exit 2; }

# --- install ALD Pro from the vendor repo -----------------------------------
# aldpro-mp is the ALD Pro metapackage; it pulls in the rest (portal, server,
# FreeIPA-based directory). The repo must already be configured on the guest
# (it ships with ALD Pro media); we do not add any third-party repository here.
if command -v aldpro-server-install >/dev/null 2>&1; then
  echo "lab: ALD Pro already installed ($(dpkg-query -W -f '${Version}' aldpro-mp 2>/dev/null || echo '?'))"
else
  echo "lab: installing ALD Pro (aldpro-mp) from the Astra ALD Pro repository"
  run sh -c "DEBIAN_FRONTEND=noninteractive apt-get update"
  run sh -c "DEBIAN_FRONTEND=noninteractive apt-get install -q -y aldpro-mp"
fi
(( DRY )) || echo "lab: aldpro-mp = $(dpkg-query -W -f '${Version}' aldpro-mp 2>/dev/null || echo '?')"

# --- promote to a controller with the vendor installer ----------------------
# Skip if a domain already exists (IPA/ALD Pro leaves a server config behind).
if (( ! DRY )) && { [[ -f /etc/ipa/default.conf ]] || systemctl is-active --quiet ipa 2>/dev/null; }; then
  echo "lab: an ALD Pro / IPA domain already exists here, keeping it"
else
  # Name resolution for the controller itself.
  if ! grep -q "$HOST.$DOMAIN" /etc/hosts; then
    run sh -c "echo '$HOSTIP $HOST.$DOMAIN $HOST' >> /etc/hosts"
  fi
  echo "lab: promoting to ALD Pro controller ($HOST.$DOMAIN on $HOSTIP)"
  run aldpro-server-install -d "$DOMAIN" -n "$HOST" -p "$ADMINPW" --ip "$HOSTIP" --no-reboot
fi

# --- evidence ---------------------------------------------------------------
if (( ! DRY )); then
  echo "lab: FreeIPA core = $(rpm -q freeipa-server 2>/dev/null || dpkg-query -W -f '${Version}' freeipa-server 2>/dev/null || echo 'n/a (ALD Pro-packaged)')"
  if command -v kinit >/dev/null && echo "$ADMINPW" | kinit admin >/dev/null 2>&1; then
    command -v ipa >/dev/null && ipa ping 2>/dev/null || true
    kdestroy >/dev/null 2>&1 || true
  fi
  echo "lab: ALD Pro provisioning done"
fi
