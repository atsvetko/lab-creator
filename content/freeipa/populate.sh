#!/usr/bin/env bash
# Populate a FreeIPA / ALD Pro server with deliberately weak objects that can be
# exercised without DNS changes or an AD trust. Runs on the IPA server itself
# (labctl pipes it over SSH); set IPA_CONTAINER to target a container instead.
# LAB ONLY (LABCTL_LAB=1). Idempotent: existing objects are kept.
set -euo pipefail
[[ "${LABCTL_LAB:-}" == "1" ]] || { echo "refusing: set LABCTL_LAB=1"; exit 2; }

NAME=${IPA_CONTAINER:-}
PASSWORD=${LAB_ADMIN_PASSWORD:-${IPA_PASSWORD:-'Lab-Admin-Pass-2026!'}}
USERPW=${LAB_USER_PASSWORD:-${IPA_USER_PASSWORD:-'Lab-User-Pass-2026!'}}
AUDITPW=${LAB_AUDIT_PASSWORD:-$USERPW}
AUDIT=${LAB_AUDIT_USER:-audit}
REALM=${LAB_REALM:-${IPA_REALM:-IPA.EXAMPLE}}
BASE=${LAB_BASEDN:-${IPA_BASEDN:-dc=ipa,dc=example}}

if [[ -n "$NAME" ]]; then
  x()   { docker exec "$NAME" bash -c "$*"; }
  try() { docker exec "$NAME" bash -c "$*" >/dev/null 2>&1 || true; }
else
  x()   { bash -c "$*"; }
  try() { bash -c "$*" >/dev/null 2>&1 || true; }
fi

x "echo '$PASSWORD' | kinit admin"

# Users and groups. New passwords expire at once in IPA; push the expiry out so
# the scan can bind as these accounts without a password change.
# The audit account testers bind as (ordinary user, its own password).
try "printf '%s\n%s\n' '$AUDITPW' '$AUDITPW' | ipa user-add $AUDIT --first=Audit --last=Lab --password"
try "ipa user-mod $AUDIT --setattr=krbPasswordExpiration=20380101000000Z"
for u in carol alice bob; do
  try "printf '%s\n%s\n' '$USERPW' '$USERPW' | ipa user-add $u --first=${u^} --last=Lab --password"
  try "ipa user-mod $u --setattr=krbPasswordExpiration=20380101000000Z"
done
try "ipa group-add ops"
try "ipa group-add-member ops --users=bob"
try "ipa group-add-member admins --users=carol"            # DSA-0105: admin with a password only
try "ipa group-add-member admins --users=alice"
try "ipa user-mod alice --user-auth-type=otp"              # alice is the counter-example

# HBAC (DSA-0101 is the default allow_all; DSA-0102 a copy under another name)
try "ipa hbacrule-add devs_everywhere --usercat=all --hostcat=all --servicecat=all --desc=temporary"

# sudo (DSA-0103, DSA-0104)
try "ipa sudorule-add root_everything --usercat=all --hostcat=all --cmdcat=all --runasusercat=all"
try "ipa sudorule-add ops_nopasswd --cmdcat=all"
try "ipa sudorule-add-user ops_nopasswd --groups=ops"
try "ipa sudorule-add-host ops_nopasswd --hosts=\$(hostname)"
try "ipa sudorule-add-option ops_nopasswd --sudooption='!authenticate'"

# Password policy and KDC switches (DSA-0107, DSA-0108, DSA-0117, DSA-0120)
try "ipa pwpolicy-mod --minlength=6 --maxfail=0"
try "ipa config-mod --enable-migration=TRUE"
try "ipa config-mod --addattr='ipaconfigstring=KDC:Disable Lockout'"
try "ipa config-mod --addattr='ipaconfigstring=KDC:Disable Default Preauth for SPNs'"

# Delegation flags and rules (DSA-0109…0112)
try "ipa host-add nfs01.ipa.example --force --ok-as-delegate=true"
try "ipa host-add web01.ipa.example --force"
try "ipa service-add HTTP/web01.ipa.example --force --ok-to-auth-as-delegate=true"
try "ipa host-add legacy.ipa.example --force"
try "ipa service-add cifs/legacy.ipa.example --force --requires-pre-auth=false"
try "ipa servicedelegationrule-add portal-to-ldap"
try "ipa servicedelegationrule-add-member portal-to-ldap --principals=HTTP/web01.ipa.example@$REALM"
try "ipa servicedelegationrule-add-target portal-to-ldap --servicedelegationtargets=ipa-ldap-delegation-targets"

# Realm: RC4 permitted, long tickets (DSA-0113, DSA-0114)
x "printf 'dn: cn=$REALM,cn=kerberos,$BASE\nchangetype: modify\nadd: krbSupportedEncSaltTypes\nkrbSupportedEncSaltTypes: arcfour-hmac:special\n' | ldapmodify -x -D 'cn=Directory Manager' -w '$PASSWORD' -H ldap://localhost >/dev/null 2>&1 || true"
try "ipa krbtpolicy-mod --maxlife=172800"

# CA ACL, RBAC (DSA-0115, DSA-0121, DSA-0122)
try "ipa caacl-add everyone_any_profile --usercat=all --profilecat=all --cacat=all"
try "ipa permission-add 'Helpdesk reset passwords' --bindtype=all --right=write --attrs=userpassword --type=user"
try "ipa role-add helpdesk"
try "ipa role-add-member helpdesk --groups=ipausers"

# Self-check
x "ipa hbacrule-find --all 2>/dev/null | grep -c 'Rule name' ; ipa pwpolicy-show | grep -E 'Min length|Max failures'; ipa config-show | grep -E 'Migration|Password plugin'"
echo "lab populated"
