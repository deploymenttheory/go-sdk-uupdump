package builder_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/builder"
	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/udf"
	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/wim"
)

// TestBuildISOWin11Bypass is the Phase-3 end-to-end guard: building with both
// bypass mechanisms yields an ISO whose install.wim images are
// INSTALLATIONTYPE=Server (mechanism A) and whose root carries an
// autounattend.xml with the LabConfig + BypassNRO keys (mechanism B).
func TestBuildISOWin11Bypass(t *testing.T) {
	esd := makeSyntheticESD(t)
	outISO := filepath.Join(t.TempDir(), "bypass.iso")

	err := builder.BuildISO(esd, outISO, builder.Options{
		VolumeID: "BYPASS",
		Bypass: builder.Win11Bypass{
			InstallationTypeServer: true,
			LabConfig:              true,
			BypassNRO:              true,
		},
	})
	if err != nil {
		t.Fatalf("BuildISO: %v", err)
	}

	f, err := os.Open(outISO)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	vol, err := udf.Read(f)
	if err != nil {
		t.Fatalf("udf.Read: %v", err)
	}

	// Mechanism A: every install.wim image is INSTALLATIONTYPE=Server.
	installWIM := readUDFFile(t, vol, []string{"sources", "install.wim"})
	if installWIM == nil {
		t.Fatal("install.wim missing from ISO")
	}
	p := filepath.Join(t.TempDir(), "install.wim")
	if err := os.WriteFile(p, installWIM, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := wim.Open(p)
	if err != nil {
		t.Fatalf("reopen install.wim: %v", err)
	}
	defer w.Close()
	if len(w.Images()) == 0 {
		t.Fatal("install.wim has no images")
	}
	for _, im := range w.Images() {
		if im.InstallationType != "Server" {
			t.Errorf("image %q InstallationType=%q, want Server", im.Name, im.InstallationType)
		}
	}

	// Mechanism B: autounattend.xml at the ISO root carries the bypass keys.
	auto := readUDFFile(t, vol, []string{"autounattend.xml"})
	if auto == nil {
		t.Fatal("autounattend.xml missing at ISO root")
	}
	for _, want := range [][]byte{[]byte("BypassTPMCheck"), []byte("BypassSecureBootCheck"), []byte("BypassNRO")} {
		if !bytes.Contains(auto, want) {
			t.Errorf("autounattend.xml missing %q", want)
		}
	}
}

// TestBuildISOBypassRespectsUserAutounattend verifies a caller-supplied
// autounattend.xml is not overwritten by the generated one.
func TestBuildISOBypassRespectsUserAutounattend(t *testing.T) {
	esd := makeSyntheticESD(t)
	outISO := filepath.Join(t.TempDir(), "user.iso")
	const userXML = "<unattend>USER</unattend>"

	err := builder.BuildISO(esd, outISO, builder.Options{
		VolumeID:   "USERAUTO",
		ExtraFiles: map[string][]byte{"autounattend.xml": []byte(userXML)},
		Bypass:     builder.Win11Bypass{LabConfig: true},
	})
	if err != nil {
		t.Fatalf("BuildISO: %v", err)
	}

	f, err := os.Open(outISO)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	vol, err := udf.Read(f)
	if err != nil {
		t.Fatalf("udf.Read: %v", err)
	}
	auto := readUDFFile(t, vol, []string{"autounattend.xml"})
	if string(auto) != userXML {
		t.Errorf("user autounattend.xml was overwritten: %q", auto)
	}
}
