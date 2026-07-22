// Package unattend generates Windows Setup answer files (autounattend.xml),
// focused on the Windows 11 requirement bypass.
//
// It complements the WIM-level bypass (setting WINDOWS/INSTALLATIONTYPE=Server,
// see pkg/wim.Updater): where that edits the image so Setup skips the whole
// hardware appraisal, this writes the Microsoft-documented LabConfig registry
// keys that individually disable the TPM, Secure Boot, RAM, storage, and CPU
// checks, plus the OOBE BypassNRO key that removes the online-account/network
// requirement. Emitting both mechanisms is belt-and-suspenders and covers the
// in-place-upgrade path that the image edit alone does not.
//
// The generated file is placed at the media root as autounattend.xml, where
// Windows Setup auto-detects it (see pkg/builder.Options.ExtraFiles).
package unattend

import (
	"fmt"
	"strings"
)

// Config selects which answer-file behaviours to emit.
type Config struct {
	// Architecture is the processorArchitecture for the Setup components:
	// "amd64" (default), "arm64", or "x86".
	Architecture string
	// LabConfig emits the windowsPE LabConfig bypass keys (BypassTPMCheck,
	// BypassSecureBootCheck, BypassRAMCheck, BypassStorageCheck, BypassCPUCheck),
	// which Windows Setup reads to skip the corresponding Windows 11 checks.
	LabConfig bool
	// BypassNRO emits the specialize-pass OOBE\BypassNRO key so first-boot OOBE
	// does not force a network connection / Microsoft account.
	BypassNRO bool
}

// labConfigKeys are the LabConfig value names that disable each Windows 11
// requirement check, in a stable order.
var labConfigKeys = []string{
	"BypassTPMCheck",
	"BypassSecureBootCheck",
	"BypassRAMCheck",
	"BypassStorageCheck",
	"BypassCPUCheck",
}

// Generate builds an autounattend.xml for the given configuration. It returns an
// error if no bypass is selected (an empty answer file would be pointless) or
// the architecture is unknown.
func Generate(cfg Config) ([]byte, error) {
	arch := cfg.Architecture
	if arch == "" {
		arch = "amd64"
	}
	switch arch {
	case "amd64", "arm64", "x86":
	default:
		return nil, fmt.Errorf("unattend: unsupported architecture %q", arch)
	}
	if !cfg.LabConfig && !cfg.BypassNRO {
		return nil, fmt.Errorf("unattend: no bypass selected")
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<unattend xmlns="urn:schemas-microsoft-com:unattend">` + "\n")

	if cfg.LabConfig {
		writeSettings(&b, "windowsPE", "Microsoft-Windows-Setup", arch, func(rs *strings.Builder) {
			for i, key := range labConfigKeys {
				writeRegAddCommand(rs, i+1,
					`HKLM\System\Setup\LabConfig`, key)
			}
		})
	}
	if cfg.BypassNRO {
		writeSettings(&b, "specialize", "Microsoft-Windows-Deployment", arch, func(rs *strings.Builder) {
			writeRegAddCommand(rs, 1,
				`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\OOBE`, "BypassNRO")
		})
	}

	b.WriteString(`</unattend>` + "\n")
	return []byte(b.String()), nil
}

// writeSettings emits a <settings pass="…"> block containing a single component
// whose <RunSynchronous> body is produced by fill.
func writeSettings(b *strings.Builder, pass, component, arch string, fill func(*strings.Builder)) {
	fmt.Fprintf(b, "  <settings pass=%q>\n", pass)
	fmt.Fprintf(b, "    <component name=%q processorArchitecture=%q publicKeyToken=\"31bf3856ad364e35\" "+
		"language=\"neutral\" versionScope=\"nonSxS\" "+
		"xmlns:wcm=\"http://schemas.microsoft.com/WMIConfig/2002/State\" "+
		"xmlns:xsi=\"http://www.w3.org/2001/XMLSchema-instance\">\n", component, arch)
	b.WriteString("      <RunSynchronous>\n")
	fill(b)
	b.WriteString("      </RunSynchronous>\n")
	b.WriteString("    </component>\n")
	b.WriteString("  </settings>\n")
}

// writeRegAddCommand emits one ordered RunSynchronousCommand that sets a
// REG_DWORD value to 1 under keyPath.
func writeRegAddCommand(b *strings.Builder, order int, keyPath, value string) {
	path := fmt.Sprintf(`reg add "%s" /v %s /t REG_DWORD /d 1 /f`, keyPath, value)
	b.WriteString("        <RunSynchronousCommand wcm:action=\"add\">\n")
	fmt.Fprintf(b, "          <Order>%d</Order>\n", order)
	fmt.Fprintf(b, "          <Path>%s</Path>\n", xmlEscape(path))
	b.WriteString("        </RunSynchronousCommand>\n")
}

// xmlEscape escapes XML text content.
func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
