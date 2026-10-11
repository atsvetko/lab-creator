#Requires -Version 5.1
#Requires -RunAsAdministrator
<#
.SYNOPSIS
  Promote this throw-away Windows Server VM to the first domain controller of a
  disposable forest, for a lab-creator test lab.

.DESCRIPTION
  LAB ONLY. This creates a new AD forest on the machine it runs on. Run it only
  on a disposable VM on an isolated Hyper-V switch. It refuses unless
  LABCTL_LAB=1. It is idempotent: if the machine is already a DC of the
  target domain it does nothing. The machine reboots at the end of a fresh
  promotion; run populate.ps1 after the reboot.

.NOTES
  No secret is stored in this file. The DSRM (Safe Mode) password is taken from
  $env:LAB_SAFEMODE_PASSWORD, or a random one is generated and discarded (the
  forest is disposable and restored from a checkpoint, so DSRM is never needed).
#>
[CmdletBinding(SupportsShouldProcess, ConfirmImpact = 'High')]
param(
    [string]$DomainName  = 'da.test',
    [string]$NetBiosName = 'DA',
    [string]$IPAddress   = '10.55.0.10',
    [int]   $PrefixLength = 24,
    [ValidateSet('Win2016','Win2025','Default')]
    [string]$ForestMode  = 'Win2016'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$logDir = Join-Path $PSScriptRoot 'logs'
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
$log = Join-Path $logDir ("provision-{0:yyyyMMdd-HHmmss}.log" -f (Get-Date))
Start-Transcript -Path $log -Append | Out-Null

try {
    if ($env:LABCTL_LAB -ne '1') {
        throw 'refusing: set LABCTL_LAB=1 (this script creates an AD forest on this machine)'
    }

    Write-Host "lab forest: $DomainName ($NetBiosName), this host becomes its first DC"

    # Idempotent: already a DC of this domain? Then stop here. Before promotion
    # the ActiveDirectory module and a running directory may both be absent, so
    # any failure here just means "not a DC yet" and we continue.
    $alreadyDC = $false
    try {
        if (Get-Command Get-ADDomain -ErrorAction SilentlyContinue) {
            $d = Get-ADDomain -ErrorAction Stop
            if ($d.DNSRoot -eq $DomainName) { $alreadyDC = $true }
            elseif ($d) { throw "this machine is already a DC of $($d.DNSRoot), not $DomainName; use a clean VM" }
        }
    } catch {
        if ("$_" -match 'already a DC of') { throw }
        # otherwise: directory not running / module not loaded — not a DC yet.
    }
    if ($alreadyDC) {
        Write-Host "already a DC of $DomainName — nothing to do"
        return
    }

    # 1. Static IP and self-pointing DNS: a DC must resolve its own domain.
    $ifIndex = (Get-NetAdapter -Physical | Where-Object Status -eq 'Up' | Select-Object -First 1).ifIndex
    if (-not $ifIndex) { throw 'no connected network adapter found' }
    if ($PSCmdlet.ShouldProcess("adapter $ifIndex", "set static IP $IPAddress/$PrefixLength, DNS 127.0.0.1")) {
        if (-not (Get-NetIPAddress -IPAddress $IPAddress -ErrorAction SilentlyContinue)) {
            Get-NetIPAddress -InterfaceIndex $ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue |
                Where-Object { $_.PrefixOrigin -ne 'WellKnown' } | Remove-NetIPAddress -Confirm:$false -ErrorAction SilentlyContinue
            New-NetIPAddress -InterfaceIndex $ifIndex -IPAddress $IPAddress -PrefixLength $PrefixLength | Out-Null
        }
        Set-DnsClientServerAddress -InterfaceIndex $ifIndex -ServerAddresses '127.0.0.1'
    }

    # 2. AD DS role.
    if ($PSCmdlet.ShouldProcess('AD-Domain-Services', 'install role')) {
        Install-WindowsFeature -Name AD-Domain-Services -IncludeManagementTools | Out-Null
    }

    # 3. DSRM password: from env, else random and discarded (disposable forest).
    if ($env:LAB_SAFEMODE_PASSWORD) {
        $safe = ConvertTo-SecureString $env:LAB_SAFEMODE_PASSWORD -AsPlainText -Force
    } else {
        $rand = -join ((48..57) + (65..90) + (97..122) + (33,35,37,42) | Get-Random -Count 24 | ForEach-Object { [char]$_ })
        $safe = ConvertTo-SecureString $rand -AsPlainText -Force
        Write-Host 'DSRM password: generated at random and not stored (restore from checkpoint instead)'
    }

    $modeMap = @{ Win2016 = 'WinThreshold'; Win2025 = 'WinThreshold'; Default = 'Default' }
    $mode = $modeMap[$ForestMode]

    if ($PSCmdlet.ShouldProcess($DomainName, 'Install-ADDSForest (promote, will reboot)')) {
        Import-Module ADDSDeployment
        Install-ADDSForest `
            -DomainName $DomainName `
            -DomainNetbiosName $NetBiosName `
            -ForestMode $mode -DomainMode $mode `
            -InstallDns:$true `
            -SafeModeAdministratorPassword $safe `
            -NoRebootOnCompletion:$false `
            -Force:$true
        Write-Host 'promotion requested; the machine will reboot. Run populate.ps1 after it comes back.'
    }
}
finally {
    Stop-Transcript | Out-Null
}
