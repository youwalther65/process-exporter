package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFakeCgroupfs builds a minimal cgroupfs tree under a temp dir and returns
// its root path. The layout mirrors a Bottlerocket runtime.slice unit.
func writeFakeCgroupfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	unit := filepath.Join(root, "runtime.slice", "containerd.service")
	if err := os.MkdirAll(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"cpu.pressure":    "some avg10=0.00 avg60=0.00 avg300=0.00 total=100\n",
		"memory.pressure": "some avg10=1.00 avg60=2.00 avg300=3.00 total=5000\nfull avg10=0.50 avg60=0.60 avg300=0.70 total=2500\n",
		"memory.current":  "104857600\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(unit, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestReaderReadPSI(t *testing.T) {
	root := writeFakeCgroupfs(t)
	r, err := NewReader(root)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	stats, err := r.ReadPSI("/runtime.slice/containerd.service", "memory")
	if err != nil {
		t.Fatalf("ReadPSI: %v", err)
	}
	if stats.Some == nil || stats.Some.Total != 5000 {
		t.Errorf("some = %+v, want total 5000", stats.Some)
	}
	if stats.Full == nil || stats.Full.Total != 2500 {
		t.Errorf("full = %+v, want total 2500", stats.Full)
	}
}

func TestReaderExists(t *testing.T) {
	root := writeFakeCgroupfs(t)
	r, err := NewReader(root)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	if !r.exists("/runtime.slice/containerd.service", "cpu.pressure") {
		t.Error("cpu.pressure should exist")
	}
	if r.exists("/runtime.slice/containerd.service", "io.pressure") {
		t.Error("io.pressure should not exist")
	}
}

// TestReaderRefusesEscape verifies os.Root confinement: a path that tries to
// climb out of the cgroupfs root must fail rather than read an outside file.
func TestReaderRefusesEscape(t *testing.T) {
	root := writeFakeCgroupfs(t)
	// A secret file one level above the root that must never be readable.
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(root)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	if _, err := r.readFile("/../", "secret.txt"); err == nil {
		t.Error("expected os.Root to refuse a path escaping the root")
	}
}

func TestReaderMissingFile(t *testing.T) {
	root := writeFakeCgroupfs(t)
	r, err := NewReader(root)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	if _, err := r.ReadPSI("/runtime.slice/containerd.service", "io"); err == nil {
		t.Error("expected error reading absent io.pressure")
	}
}
