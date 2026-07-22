package diskspace

import (
	"errors"
	"math"
	"path/filepath"
	"testing"
)

func TestAvailablePositive(t *testing.T) {
	got, err := Available(t.TempDir())
	if err != nil {
		t.Fatalf("Available: %v", err)
	}
	if got == 0 {
		t.Error("expected non-zero free space on the temp volume")
	}
}

func TestAvailableNonexistentPathUsesAncestor(t *testing.T) {
	// A path that does not exist yet (an output file under a temp dir) must still
	// resolve via its existing ancestor.
	p := filepath.Join(t.TempDir(), "sub", "not", "created", "out.iso")
	got, err := Available(p)
	if err != nil {
		t.Fatalf("Available(nonexistent): %v", err)
	}
	if got == 0 {
		t.Error("expected non-zero free space via ancestor")
	}
}

func TestEnsureAvailable(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureAvailable(dir, 1); err != nil {
		t.Errorf("1 byte should be available: %v", err)
	}
	err := EnsureAvailable(dir, math.MaxUint64)
	var ise *InsufficientSpaceError
	if !errors.As(err, &ise) {
		t.Fatalf("expected InsufficientSpaceError, got %v", err)
	}
	if ise.Need != math.MaxUint64 {
		t.Errorf("error Need = %d", ise.Need)
	}
}

func TestSameVolume(t *testing.T) {
	d := t.TempDir()
	if !SameVolume(d, d) {
		t.Error("a path should share a volume with itself")
	}
	// A not-yet-existing subpath resolves to its existing ancestor, so it shares
	// that volume.
	if !SameVolume(d, filepath.Join(d, "a", "b", "c.iso")) {
		t.Error("a subpath should share its ancestor's volume")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{
		0:             "0 B",
		512:           "512 B",
		1500:          "1.50 kB",
		4_680_000_000: "4.68 GB",
	}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
