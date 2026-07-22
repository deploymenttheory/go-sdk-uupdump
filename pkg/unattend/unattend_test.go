package unattend

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestGenerateWellFormed(t *testing.T) {
	out, err := Generate(Config{LabConfig: true, BypassNRO: true})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Must be well-formed XML.
	if err := xml.Unmarshal(out, new(struct{})); err != nil {
		t.Fatalf("generated autounattend is not well-formed XML: %v", err)
	}
	s := string(out)
	// Every LabConfig bypass key present, targeting the LabConfig hive path.
	for _, key := range labConfigKeys {
		if !strings.Contains(s, key) {
			t.Errorf("missing LabConfig key %q", key)
		}
	}
	if !strings.Contains(s, `HKLM\System\Setup\LabConfig`) {
		t.Error("missing LabConfig registry path")
	}
	// BypassNRO in the specialize pass targeting the OOBE hive.
	if !strings.Contains(s, `pass="specialize"`) || !strings.Contains(s, "BypassNRO") {
		t.Error("missing BypassNRO / specialize pass")
	}
	if !strings.Contains(s, `pass="windowsPE"`) {
		t.Error("missing windowsPE pass")
	}
	if !strings.Contains(s, `processorArchitecture="amd64"`) {
		t.Error("default architecture should be amd64")
	}
}

func TestGenerateArchAndErrors(t *testing.T) {
	if _, err := Generate(Config{}); err == nil {
		t.Error("expected error when no bypass selected")
	}
	if _, err := Generate(Config{LabConfig: true, Architecture: "sparc"}); err == nil {
		t.Error("expected error for unknown architecture")
	}
	out, err := Generate(Config{LabConfig: true, Architecture: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `processorArchitecture="arm64"`) {
		t.Error("arm64 architecture not honoured")
	}
}

func TestGenerateLabConfigOnlyOmitsSpecialize(t *testing.T) {
	out, err := Generate(Config{LabConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `pass="specialize"`) {
		t.Error("specialize pass should be absent without BypassNRO")
	}
}
