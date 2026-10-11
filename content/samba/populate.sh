#!/usr/bin/env bash
# Populate a freshly provisioned Samba AD DC with deliberately weak objects so
# directory scanners have known weaknesses to find.
#
# LAB ONLY. Every change here is a misconfiguration on purpose. The script
# refuses to run unless LABCTL_LAB=1 is set, and it is idempotent: each
# step tolerates "already exists".
#
# Usage: LABCTL_LAB=1 populate.sh [--dry-run]
set -euo pipefail

[[ "${LABCTL_LAB:-}" == "1" ]] || { echo "refusing: set LABCTL_LAB=1 (this script weakens a domain)"; exit 2; }
DRY=0; [[ "${1:-}" == "--dry-run" ]] && DRY=1

PY=${SAMBA_PYTHON:-python3}
ST="$PY /usr/bin/samba-tool"
SAM=${SAM_LDB:-/var/lib/samba/private/sam.ldb}
B=${LAB_BASEDN:-DC=lab,DC=example}
PW=${LAB_USER_PASSWORD:-'Lab-User-Pass-2026!'}
AUDIT=${LAB_AUDIT_USER:-audit}
AUDITPW=${LAB_AUDIT_PASSWORD:-$PW}

run() { if (( DRY )); then echo "+ $*"; else "$@"; fi; }
try() { if (( DRY )); then echo "+ $*"; else "$@" >/dev/null 2>&1 || true; fi; }

# Accounts. The audit account testers bind as is a plain member of Domain Users.
try $ST user create "$AUDIT" "$AUDITPW"
for u in svc-backup j.legacy old.admin helpdesk1; do try $ST user create "$u" "$PW"; done
try $ST group add Helpdesk
try $ST group add "Tier0 Ops"
try $ST computer create APP01
try $ST computer create WS01

# Nested Tier 0: svc-backup → Tier0 Ops → Domain Admins; it also carries an SPN (DSA-0002)
try $ST group addmembers "Tier0 Ops" svc-backup
try $ST group addmembers "Domain Admins" "Tier0 Ops"
try $ST spn add MSSQLSvc/db01.lab.example:1433 svc-backup
try $ST group addmembers Helpdesk helpdesk1

# Orphaned adminCount (DSA-0019): in and out of Domain Admins, SDProp has run in between on a real DC;
# here we set adminCount directly to make the state deterministic.
try $ST group addmembers "Domain Admins" old.admin
try $ST group removemembers "Domain Admins" old.admin

LDIF=$(mktemp)
cat > "$LDIF" <<EOF
dn: CN=j.legacy,CN=Users,$B
changetype: modify
replace: userAccountControl
userAccountControl: 4194944

dn: CN=APP01,CN=Computers,$B
changetype: modify
replace: userAccountControl
userAccountControl: 528384

dn: CN=old.admin,CN=Users,$B
changetype: modify
replace: adminCount
adminCount: 1

dn: CN=Directory Service,CN=Windows NT,CN=Services,CN=Configuration,$B
changetype: modify
replace: dSHeuristics
dSHeuristics: 0000002
EOF
# j.legacy: NORMAL_ACCOUNT | ENCRYPTED_TEXT_PWD_ALLOWED (0x80) | DONT_REQ_PREAUTH (0x400000)  (DSA-0001, DSA-0011)
# APP01:    WORKSTATION_TRUST_ACCOUNT | TRUSTED_FOR_DELEGATION (0x80000)                       (DSA-0004)
# dSHeuristics 7th char = 2: anonymous LDAP operations                                        (DSA-0025)
run ldbmodify -H "$SAM" "$LDIF" >/dev/null
rm -f "$LDIF"

# Shadow admin (DSA-0015): Helpdesk gets GenericAll on Domain Admins
HSID=$(ldbsearch -H "$SAM" "(sAMAccountName=Helpdesk)" objectSid 2>/dev/null | awk '/^objectSid/{print $2}')
[[ -n "$HSID" ]] || { echo "Helpdesk SID not found"; exit 1; }
run $ST dsacl set --objectdn="CN=Domain Admins,CN=Users,$B" --sddl="(A;;GA;;;$HSID)" >/dev/null

# Weak domain policy (DSA-0020…0023)
run $ST domain passwordsettings set --complexity=off --history-length=0 --max-pwd-age=0 --min-pwd-length=7 --account-lockout-threshold=0 >/dev/null

# Self-check: print the resulting state
if (( ! DRY )); then
  echo "policy:"; $ST domain passwordsettings show | grep -E "complexity|history|Maximum|lockout threshold"
  echo "j.legacy UAC: $(ldbsearch -H "$SAM" "(sAMAccountName=j.legacy)" userAccountControl 2>/dev/null | awk '/^userAccountControl/{print $2}')"
  echo "dSHeuristics: $(ldbsearch -H "$SAM" -b "CN=Directory Service,CN=Windows NT,CN=Services,CN=Configuration,$B" -s base dSHeuristics 2>/dev/null | awk '/^dSHeuristics/{print $2}')"
  echo "lab populated"
fi
