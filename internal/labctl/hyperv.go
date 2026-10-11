package labctl

import (
	"context"
	"fmt"
	"net"
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
	vhd := filepath.Join(l.Work(), "vhd", m.Name+".vhdx")
	secondary := ""
	if answerISO != "" {
		secondary = fmt.Sprintf("Add-VMDvdDrive -VMName %s -Path %s", psQuote(m.Name), psQuote(answerISO))
	}
	// Firmware depends on the plugin: Windows and UEFI Linux use Gen-2 with
	// Secure Boot off; a plugin may ask for Gen-1 for an older Linux installer.
	gen, secureBoot := 2, false
	if p, ok := plugin(m.Kind); ok {
		if p.Generation != 0 {
			gen = p.Generation
		}
		secureBoot = p.SecureBoot
	}
	firmware, bootDev := "", ""
	if gen == 2 {
		sb := "Off"
		if secureBoot {
			sb = "On"
		}
		firmware = fmt.Sprintf("Set-VMFirmware -VMName %s -EnableSecureBoot %s\n", psQuote(m.Name), sb)
		bootDev = fmt.Sprintf("$dvd = Get-VMDvdDrive -VMName %s | Select-Object -First 1\nSet-VMFirmware -VMName %s -FirstBootDevice $dvd\n", psQuote(m.Name), psQuote(m.Name))
	}
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$vhd = %s
New-Item -ItemType Directory -Force -Path (Split-Path $vhd) | Out-Null
if (Get-VM -Name %s -ErrorAction SilentlyContinue) { throw 'VM already exists; remove it first' }
New-VHD -Path $vhd -SizeBytes %dGB -Dynamic | Out-Null
New-VM -Name %s -Generation %d -MemoryStartupBytes %dGB -SwitchName %s -VHDPath $vhd | Out-Null
Set-VM -Name %s -ProcessorCount %d -AutomaticCheckpointsEnabled $false
%sAdd-VMDvdDrive -VMName %s -Path %s
%s%sWrite-Host ("created VM {0} (gen %d, {1} vCPU, {2} GB) on {3}" -f %s, %d, %d, %s)
`,
		psQuote(vhd),
		psQuote(m.Name),
		m.DiskGB,
		psQuote(m.Name), gen, m.MemGB, psQuote(l.Switch),
		psQuote(m.Name), m.CPU,
		firmware, psQuote(m.Name), psQuote(m.ISO),
		secondary, bootDev, gen,
		psQuote(m.Name), m.CPU, m.MemGB, psQuote(l.Switch))
	if err := ensureSwitch(ctx, j, l); err != nil {
		return err
	}
	if err := ps(ctx, j, script); err != nil {
		return err
	}
	return ps(ctx, j, fmt.Sprintf("Start-VM -Name %s", psQuote(m.Name)))
}

// ensureSwitch creates the lab's internal switch if it is missing and gives the
// host an address on it (the first host address of the lab subnet), so labctl
// can reach Linux guests over SSH. An existing switch is left alone; a switch
// of any other type is refused, because the lab is deliberately weak and must
// not be bridged to a real network.
func ensureSwitch(ctx context.Context, j *Job, l *Lab) error {
	hostIP, prefix := hostAddr(l.Subnet)
	ipStep := ""
	if hostIP != "" {
		ipStep = fmt.Sprintf(`
  $nic = Get-NetAdapter -Name ("vEthernet ({0})" -f $sw) -ErrorAction SilentlyContinue
  if ($nic -and -not (Get-NetIPAddress -InterfaceIndex $nic.ifIndex -IPAddress %s -ErrorAction SilentlyContinue)) {
    New-NetIPAddress -InterfaceIndex $nic.ifIndex -IPAddress %s -PrefixLength %d | Out-Null
    Write-Host ("host address %s/%d on {0}" -f $sw)
  }`, psQuote(hostIP), psQuote(hostIP), prefix, hostIP, prefix)
	}
	return ps(ctx, j, fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$sw = %s
$s = Get-VMSwitch -Name $sw -ErrorAction SilentlyContinue
if ($s -and $s.SwitchType -ne 'Internal' -and $s.SwitchType -ne 'Private') {
  throw ("switch {0} is {1}; lab switches must be Internal or Private (isolated)" -f $sw, $s.SwitchType)
}
if (-not $s) {
  New-VMSwitch -Name $sw -SwitchType Internal | Out-Null
  Write-Host ("created internal switch {0}" -f $sw)%s
}
`, psQuote(l.Switch), ipStep))
}

// hostAddr returns the first host address of a CIDR (10.10.30.0/24 →
// 10.10.30.1, 24), or "" when the subnet is empty or not IPv4.
func hostAddr(cidr string) (string, int) {
	_, n, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil || n.IP.To4() == nil {
		return "", 0
	}
	ip := n.IP.To4()
	first := net.IPv4(ip[0], ip[1], ip[2], ip[3]+1)
	ones, _ := n.Mask.Size()
	return first.String(), ones
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
