package labctl

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// psExe returns the PowerShell executable to use, preferring PowerShell 7.
func psExe() string {
	for _, c := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// HostOK reports whether this machine can drive Hyper-V (Windows + PowerShell).
func HostOK() (bool, string) {
	if runtime.GOOS != "windows" {
		return false, "Hyper-V actions run only on the Windows host; this build is for development of the UI."
	}
	if psExe() == "" {
		return false, "PowerShell not found on PATH."
	}
	return true, ""
}

// ps runs a PowerShell script, streaming its output into the job.
func ps(ctx context.Context, j *Job, script string) error {
	exe := psExe()
	if exe == "" {
		return fmt.Errorf("PowerShell not available (run labctl on the Hyper-V host)")
	}
	return run(ctx, j, "", exe, "-NoProfile", "-NonInteractive", "-Command", script)
}

// VMState returns the Hyper-V state of a VM ("Running", "Off", "" when absent).
func VMState(ctx context.Context, name string) string {
	exe := psExe()
	if exe == "" {
		return ""
	}
	out, err := capture(ctx, "", exe, "-NoProfile", "-NonInteractive", "-Command",
		fmt.Sprintf("(Get-VM -Name %s -ErrorAction SilentlyContinue).State", psQuote(name)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// HasCheckpoint reports whether the named checkpoint exists on a VM.
func HasCheckpoint(ctx context.Context, vm, snapshot string) bool {
	exe := psExe()
	if exe == "" {
		return false
	}
	out, _ := capture(ctx, "", exe, "-NoProfile", "-NonInteractive", "-Command",
		fmt.Sprintf("[bool](Get-VMSnapshot -VMName %s -Name %s -ErrorAction SilentlyContinue)", psQuote(vm), psQuote(snapshot)))
	return strings.EqualFold(strings.TrimSpace(out), "True")
}

// createVM builds a new Generation-2 VM from an install ISO on the lab switch,
// with a blank system disk, optionally mounting an unattended-answer ISO as a
// second drive. It does not wait for the OS install to finish.
func createVM(ctx context.Context, j *Job, l *Lab, m *Machine, answerISO string) error {
	if m.ISO == "" {
		return fmt.Errorf("%s: no install ISO set", m.Name)
	}
	vhd := filepath.Join(l.WorkDir, "vhd", m.Name+".vhdx")
	secondary := ""
	if answerISO != "" {
		secondary = fmt.Sprintf("Add-VMDvdDrive -VMName %s -Path %s", psQuote(m.Name), psQuote(answerISO))
	}
	// Gen-2 for Windows; Gen-1 is friendlier for some Linux installers, but Альт
	// (UEFI) installs fine on Gen-2 with Secure Boot off.
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$vhd = %s
New-Item -ItemType Directory -Force -Path (Split-Path $vhd) | Out-Null
if (Get-VM -Name %s -ErrorAction SilentlyContinue) { throw 'VM already exists; remove it first' }
New-VHD -Path $vhd -SizeBytes %dGB -Dynamic | Out-Null
New-VM -Name %s -Generation 2 -MemoryStartupBytes %dGB -SwitchName %s -VHDPath $vhd | Out-Null
Set-VM -Name %s -ProcessorCount %d -AutomaticCheckpointsEnabled $false
Set-VMFirmware -VMName %s -EnableSecureBoot Off
Add-VMDvdDrive -VMName %s -Path %s
%s
$dvd = Get-VMDvdDrive -VMName %s | Select-Object -First 1
Set-VMFirmware -VMName %s -FirstBootDevice $dvd
Write-Host ("created VM {0} ({1} vCPU, {2} GB, {3} GB disk) on {4}" -f %s, %d, %d, %d, %s)
`,
		psQuote(vhd),
		psQuote(m.Name),
		m.DiskGB,
		psQuote(m.Name), m.MemGB, psQuote(l.Switch),
		psQuote(m.Name), m.CPU,
		psQuote(m.Name),
		psQuote(m.Name), psQuote(m.ISO),
		secondary,
		psQuote(m.Name),
		psQuote(m.Name),
		psQuote(m.Name), m.CPU, m.MemGB, m.DiskGB, psQuote(l.Switch))
	if err := ps(ctx, j, script); err != nil {
		return err
	}
	return ps(ctx, j, fmt.Sprintf("Start-VM -Name %s", psQuote(m.Name)))
}

func startVM(ctx context.Context, j *Job, name string) error {
	return ps(ctx, j, fmt.Sprintf("Start-VM -Name %s -ErrorAction SilentlyContinue", psQuote(name)))
}

func stopVM(ctx context.Context, j *Job, name string) error {
	return ps(ctx, j, fmt.Sprintf("Stop-VM -Name %s -Force -ErrorAction SilentlyContinue", psQuote(name)))
}

func snapshotVM(ctx context.Context, j *Job, name, snapshot string) error {
	return ps(ctx, j, fmt.Sprintf(
		"Get-VMSnapshot -VMName %s -Name %s -ErrorAction SilentlyContinue | Remove-VMSnapshot -Confirm:$false; Checkpoint-VM -Name %s -SnapshotName %s",
		psQuote(name), psQuote(snapshot), psQuote(name), psQuote(snapshot)))
}

func restoreVM(ctx context.Context, j *Job, name, snapshot string) error {
	return ps(ctx, j, fmt.Sprintf(
		"$s = Get-VMSnapshot -VMName %s -Name %s -ErrorAction Stop; Restore-VMSnapshot -VMSnapshot $s -Confirm:$false; Start-VM -Name %s -ErrorAction SilentlyContinue",
		psQuote(name), psQuote(snapshot), psQuote(name)))
}

// psQuote single-quotes a value for PowerShell (doubling embedded quotes).
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
