package labctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// answerISO prepares an unattended-install answer medium for a machine and
// returns the ISO path to attach, or "" when no automated answer is available
// (the OS is then installed interactively through the Hyper-V console, after
// which Provision takes over). Windows gets an autounattend.xml packaged with
// oscdimg when the Windows ADK is present; Альт (preseed/kickstart varies by
// build) is left to an interactive install in this version.
func answerISO(ctx context.Context, j *Job, l *Lab, m *Machine, cr Creds) (string, error) {
	if m.Kind != "windows-dc" {
		j.Logf("%s: no automated answer file for kind %q — install the OS via the console, then Provision", m.Name, m.Kind)
		return "", nil
	}
	dir := filepath.Join(l.WorkDir, "unattend", m.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	xml := winAutounattend(m, cr.AdminPassword)
	if err := os.WriteFile(filepath.Join(dir, "autounattend.xml"), []byte(xml), 0o644); err != nil {
		return "", err
	}
	// Package to an ISO with oscdimg (Windows ADK) if available.
	oscd, err := exec.LookPath("oscdimg")
	if err != nil {
		j.Logf("%s: oscdimg (Windows ADK) not found — install the OS via the console, then Provision", m.Name)
		return "", nil
	}
	iso := filepath.Join(l.WorkDir, "unattend", m.Name+".iso")
	if err := run(ctx, j, "", oscd, "-n", "-m", dir, iso); err != nil {
		return "", err
	}
	j.Logf("%s: packaged autounattend.iso", m.Name)
	return iso, nil
}

// winAutounattend returns a minimal Windows Server autounattend.xml that does an
// unattended install into the first disk, sets the admin password and hostname,
// and enables auto-logon once. It intentionally does not promote the DC — that
// is Provision's job, so the same path works for a hand-installed VM.
func winAutounattend(m *Machine, adminPw string) string {
	host := m.Name
	if len(host) > 15 {
		host = host[:15]
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend">
  <settings pass="windowsPE">
    <component name="Microsoft-Windows-Setup" processorArchitecture="amd64"
        publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"
        xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <DiskConfiguration>
        <Disk wcm:action="add">
          <DiskID>0</DiskID><WillWipeDisk>true</WillWipeDisk>
          <CreatePartitions>
            <CreatePartition wcm:action="add"><Order>1</Order><Type>EFI</Type><Size>200</Size></CreatePartition>
            <CreatePartition wcm:action="add"><Order>2</Order><Type>MSR</Type><Size>128</Size></CreatePartition>
            <CreatePartition wcm:action="add"><Order>3</Order><Type>Primary</Type><Extend>true</Extend></CreatePartition>
          </CreatePartitions>
        </Disk>
      </DiskConfiguration>
      <ImageInstall><OSImage>
        <InstallTo><DiskID>0</DiskID><PartitionID>3</PartitionID></InstallTo>
        <InstallToAvailablePartition>false</InstallToAvailablePartition>
      </OSImage></ImageInstall>
      <UserData><ProductKey><Key></Key></ProductKey><AcceptEula>true</AcceptEula></UserData>
    </component>
  </settings>
  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64"
        publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"
        xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <ComputerName>%s</ComputerName>
    </component>
  </settings>
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64"
        publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"
        xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <UserAccounts><AdministratorPassword><Value>%s</Value><PlainText>true</PlainText></AdministratorPassword></UserAccounts>
      <AutoLogon><Enabled>true</Enabled><LogonCount>1</LogonCount><Username>Administrator</Username>
        <Password><Value>%s</Value><PlainText>true</PlainText></Password></AutoLogon>
      <OOBE><HideEULAPage>true</HideEULAPage><SkipMachineOOBE>true</SkipMachineOOBE><SkipUserOOBE>true</SkipUserOOBE></OOBE>
    </component>
  </settings>
</unattend>
`, host, adminPw, adminPw)
}
