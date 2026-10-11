#Requires -Version 5.1
#Requires -RunAsAdministrator
<#
.SYNOPSIS
  Seed the disposable lab DC with deliberately weak objects, so directory
  scanners have known weaknesses to find.

.DESCRIPTION
  LAB ONLY. Creates vulnerable users, groups, delegations, a weak password
  policy and a loose GPO. Refuses unless LABCTL_LAB=1. Idempotent: an
  object that already exists is reused. Everything lands in the lab domain's
  default containers so the DNs match the expectation file.

  The passwords here are throw-away lab values on an isolated forest, not real
  secrets. The "cleartext in description" value is the vulnerability under test
  (DSA-0028), not a credential that protects anything.
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [string]$DomainName   = 'da.test',
    # Audit account the CI scan authenticates as (ordinary user, no admin rights).
    [string]$AuditUser    = 'audit',
    [string]$AuditPassword = $(if ($env:LAB_AUDIT_PASSWORD) { $env:LAB_AUDIT_PASSWORD } else { 'Lab-Audit-Pass-2026!' })
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$logDir = Join-Path $PSScriptRoot 'logs'
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
$log = Join-Path $logDir ("populate-{0:yyyyMMdd-HHmmss}.log" -f (Get-Date))
Start-Transcript -Path $log -Append | Out-Null

try {
    if ($env:LABCTL_LAB -ne '1') {
        throw 'refusing: set LABCTL_LAB=1 (this script weakens the lab domain)'
    }
    Import-Module ActiveDirectory
    $dse  = Get-ADRootDSE
    $base = $dse.defaultNamingContext
    if ((Get-ADDomain).DNSRoot -ne $DomainName) { throw "this DC is not $DomainName" }
    $users = "CN=Users,$base"
    Write-Host "seeding lab objects in $base"

    $pw  = ConvertTo-SecureString 'P@ssw0rd-Lab-2026!' -AsPlainText -Force
    $wkpw = ConvertTo-SecureString 'weak' -AsPlainText -Force  # only used where a weak policy allows it

    function New-LabUser {
        param([string]$Name, [hashtable]$Extra = @{}, [securestring]$Password = $pw, [switch]$NoPassword)
        if (Get-ADUser -Filter "sAMAccountName -eq '$Name'" -ErrorAction SilentlyContinue) {
            Write-Host "  user $Name exists"
            $u = Get-ADUser -Identity $Name
        } elseif ($PSCmdlet.ShouldProcess($Name, 'New-ADUser')) {
            $p = @{ Name = $Name; SamAccountName = $Name; Path = $users; Enabled = $true;
                    AccountPassword = $Password; PasswordNeverExpires = $true }
            if ($NoPassword) { $p.Remove('AccountPassword') }
            New-ADUser @p
            $u = Get-ADUser -Identity $Name
        }
        if ($Extra.Count -and $PSCmdlet.ShouldProcess($Name, 'Set attributes')) {
            Set-ADUser -Identity $Name -Replace $Extra
        }
        return $u
    }

    # --- The audit account the scan uses (ordinary user) --------------------
    if (-not (Get-ADUser -Filter "sAMAccountName -eq '$AuditUser'" -ErrorAction SilentlyContinue)) {
        if ($PSCmdlet.ShouldProcess($AuditUser, 'New-ADUser (audit)')) {
            New-ADUser -Name $AuditUser -SamAccountName $AuditUser -Path $users -Enabled $true `
                -AccountPassword (ConvertTo-SecureString $AuditPassword -AsPlainText -Force) -PasswordNeverExpires $true
        }
    }

    # --- Account-level fixtures --------------------------------------------
    # DSA-0026 Kerberoastable: a user with an SPN.
    $svcSql = New-LabUser 'svc_sql'
    if ($PSCmdlet.ShouldProcess('svc_sql', 'add SPN')) {
        Set-ADUser -Identity 'svc_sql' -ServicePrincipalNames @{Add='MSSQLSvc/db01.da.test:1433'}
    }
    # DSA-0001 no Kerberos pre-auth (UAC DONT_REQ_PREAUTH 0x400000).
    New-LabUser 'svc_legacy' @{} | Out-Null
    Set-ADAccountControl -Identity 'svc_legacy' -DoesNotRequirePreAuth $true
    # DSA-0011 reversible encryption.
    New-LabUser 'rev_user' @{} | Out-Null
    Set-ADAccountControl -Identity 'rev_user' -AllowReversiblePasswordEncryption $true
    # DSA-0012 DES only.
    New-LabUser 'des_user' @{} | Out-Null
    Set-ADAccountControl -Identity 'des_user' -UseDESKeyOnly $true
    # DSA-0028 secret in a readable attribute (the value is the test, not a real secret).
    New-LabUser 't_contractor' @{ description = 'temp account, User Password: Welcome1!' } | Out-Null
    # DSA-0041 password never expires (set on an obviously named account; others set it too but this one is explicit).
    New-LabUser 'neverexpire_user' @{} | Out-Null

    # DSA-0004 unconstrained delegation on a non-DC account.
    if (-not (Get-ADComputer -Filter "Name -eq 'APP01'" -ErrorAction SilentlyContinue)) {
        if ($PSCmdlet.ShouldProcess('APP01$', 'New-ADComputer with unconstrained delegation')) {
            New-ADComputer -Name 'APP01' -SamAccountName 'APP01$' -Path "CN=Computers,$base" -Enabled $true `
                -TrustedForDelegation $true
        }
    } else { Set-ADComputer -Identity 'APP01$' -TrustedForDelegation $true }

    # --- Group fixtures -----------------------------------------------------
    # DSA-0027 DnsAdmins has a member.
    if (Get-ADGroup -Filter "Name -eq 'DnsAdmins'" -ErrorAction SilentlyContinue) {
        Add-ADGroupMember -Identity 'DnsAdmins' -Members 'svc_legacy' -ErrorAction SilentlyContinue
    }
    # DSA-0042 built-in Guest enabled.
    if ($PSCmdlet.ShouldProcess('Guest', 'enable')) { Enable-ADAccount -Identity 'Guest' }
    # DSA-0019 orphaned adminCount: add to and then remove from a protected group.
    New-LabUser 'ex_admin' @{} | Out-Null
    if ($PSCmdlet.ShouldProcess('ex_admin', 'add then remove from Domain Admins')) {
        Add-ADGroupMember -Identity 'Domain Admins' -Members 'ex_admin' -ErrorAction SilentlyContinue
        Start-Sleep -Seconds 2
        Remove-ADGroupMember -Identity 'Domain Admins' -Members 'ex_admin' -Confirm:$false -ErrorAction SilentlyContinue
    }

    # DSA-0015 / DSA-0061 shadow admin + control path: a non-Tier-0 user with GenericAll on Domain Admins.
    New-LabUser 'helpdesk' @{} | Out-Null
    if ($PSCmdlet.ShouldProcess('Domain Admins', 'grant helpdesk GenericAll')) {
        $daDn = (Get-ADGroup 'Domain Admins').DistinguishedName
        $sid  = (Get-ADUser 'helpdesk').SID
        $acl  = Get-Acl -Path "AD:$daDn"
        $ace  = New-Object System.DirectoryServices.ActiveDirectoryAccessRule(
                    $sid, 'GenericAll', 'Allow', [DirectoryServices.ActiveDirectorySecurityInheritance]::None)
        $acl.AddAccessRule($ace)
        Set-Acl -Path "AD:$daDn" -AclObject $acl
    }

    # --- DSA-0071 weak fine-grained password policy on Domain Admins --------
    if (-not (Get-ADFineGrainedPasswordPolicy -Filter "Name -eq 'WeakAdminPSO'" -ErrorAction SilentlyContinue)) {
        if ($PSCmdlet.ShouldProcess('WeakAdminPSO', 'New-ADFineGrainedPasswordPolicy minlen 6')) {
            New-ADFineGrainedPasswordPolicy -Name 'WeakAdminPSO' -Precedence 10 `
                -MinPasswordLength 6 -ComplexityEnabled $false
            Add-ADFineGrainedPasswordPolicySubject -Identity 'WeakAdminPSO' -Subjects 'Domain Admins'
        }
    }

    # --- DSA-0072 a GPO linked to the domain, editable by a non-Tier-0 user --
    Import-Module GroupPolicy
    $gpoName = 'DA-Lab-Weak-GPO'
    $gpo = Get-GPO -Name $gpoName -ErrorAction SilentlyContinue
    if (-not $gpo -and $PSCmdlet.ShouldProcess($gpoName, 'New-GPO + link to domain + grant helpdesk edit')) {
        $gpo = New-GPO -Name $gpoName
        New-GPLink -Name $gpoName -Target $base -ErrorAction SilentlyContinue | Out-Null
        Set-GPPermission -Name $gpoName -TargetName 'helpdesk' -TargetType User -PermissionLevel GpoEditDeleteModifySecurity | Out-Null
    }

    Write-Host ''
    Write-Host 'self-check — objects created:'
    'svc_sql','svc_legacy','rev_user','des_user','t_contractor','neverexpire_user','ex_admin','helpdesk',$AuditUser |
        ForEach-Object { '  user {0}: {1}' -f $_, [bool](Get-ADUser -Filter "sAMAccountName -eq '$_'" -ErrorAction SilentlyContinue) }
    '  computer APP01$: {0}' -f [bool](Get-ADComputer -Filter "Name -eq 'APP01'" -ErrorAction SilentlyContinue)
    '  PSO WeakAdminPSO: {0}' -f [bool](Get-ADFineGrainedPasswordPolicy -Filter "Name -eq 'WeakAdminPSO'" -ErrorAction SilentlyContinue)
    '  GPO {0}: {1}' -f $gpoName, [bool](Get-GPO -Name $gpoName -ErrorAction SilentlyContinue)
    Write-Host 'done. Take the Hyper-V checkpoint "vuln-baseline" now (see README).'
}
finally {
    Stop-Transcript | Out-Null
}
