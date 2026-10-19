package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeKubepodsTree builds a cgroupfs tree shaped like a Kubernetes node: two
// QoS parents with pod cgroups under them, each pod holding one container
// cgroup, plus an interface file at every level so the expander has to tell
// directories from files.
func writeKubepodsTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dirs := []string{
		"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice/cri-containerd-1.scope",
		"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podbbb.slice/cri-containerd-2.scope",
		"kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podccc.slice",
		"runtime.slice/containerd.service",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// memory.current exists at every level of a real tree; writing it here
	// means a wildcard that wrongly matched a file would be visible.
	files := []string{
		"kubepods.slice/memory.current",
		"kubepods.slice/kubepods-burstable.slice/memory.current",
		"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice/memory.current",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(root, f), []byte("1024\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func newTreeReader(t *testing.T) *Reader {
	t.Helper()
	r, err := NewReader(writeKubepodsTree(t))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func equalPaths(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestExpandPatternLiteral(t *testing.T) {
	r := newTreeReader(t)

	got, err := r.ExpandPattern("/kubepods.slice", 0)
	if err != nil {
		t.Fatalf("ExpandPattern: %v", err)
	}
	if want := []string{"/kubepods.slice"}; !equalPaths(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A literal path that does not exist yields no paths rather than a path that
// would produce only read errors downstream.
func TestExpandPatternLiteralMissing(t *testing.T) {
	r := newTreeReader(t)

	got, err := r.ExpandPattern("/nosuch.slice", 0)
	if err != nil {
		t.Fatalf("ExpandPattern: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

func TestExpandPatternOneLevel(t *testing.T) {
	r := newTreeReader(t)

	got, err := r.ExpandPattern("/kubepods.slice/*", 0)
	if err != nil {
		t.Fatalf("ExpandPattern: %v", err)
	}
	want := []string{
		"/kubepods.slice/kubepods-besteffort.slice",
		"/kubepods.slice/kubepods-burstable.slice",
	}
	if !equalPaths(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Two wildcard levels reach the pod cgroups, which is what a per-pod refault
// measurement needs. The memory.current files alongside them must not match.
func TestExpandPatternTwoLevels(t *testing.T) {
	r := newTreeReader(t)

	got, err := r.ExpandPattern("/kubepods.slice/*/*", 0)
	if err != nil {
		t.Fatalf("ExpandPattern: %v", err)
	}
	want := []string{
		"/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podccc.slice",
		"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice",
		"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podbbb.slice",
	}
	if !equalPaths(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A wildcard mixed with a literal segment selects one QoS class.
func TestExpandPatternLiteralAndWildcard(t *testing.T) {
	r := newTreeReader(t)

	got, err := r.ExpandPattern("/kubepods.slice/kubepods-burstable.slice/*", 0)
	if err != nil {
		t.Fatalf("ExpandPattern: %v", err)
	}
	want := []string{
		"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice",
		"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podbbb.slice",
	}
	if !equalPaths(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A glob with a prefix narrows to one QoS class by name.
func TestExpandPatternPrefixGlob(t *testing.T) {
	r := newTreeReader(t)

	got, err := r.ExpandPattern("/kubepods.slice/kubepods-burstable.slice/*-podaaa.slice", 0)
	if err != nil {
		t.Fatalf("ExpandPattern: %v", err)
	}
	want := []string{"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podaaa.slice"}
	if !equalPaths(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Over the limit the whole pattern fails, because emitting a truncated set
// would silently leave cgroups out of any sum over the label.
func TestExpandPatternTooMany(t *testing.T) {
	r := newTreeReader(t)

	got, err := r.ExpandPattern("/kubepods.slice/*/*", 2)
	if !errors.Is(err, ErrTooManyMatches) {
		t.Fatalf("err = %v, want ErrTooManyMatches", err)
	}
	if got != nil {
		t.Errorf("got %v, want no paths on the error path", got)
	}
}

// The limit applies at the level it is exceeded, not only to the final result.
func TestExpandPatternLimitAtFirstLevel(t *testing.T) {
	r := newTreeReader(t)

	if _, err := r.ExpandPattern("/kubepods.slice/*", 1); !errors.Is(err, ErrTooManyMatches) {
		t.Fatalf("err = %v, want ErrTooManyMatches", err)
	}
}

// A limit exactly equal to the number of matches is allowed.
func TestExpandPatternLimitExact(t *testing.T) {
	r := newTreeReader(t)

	got, err := r.ExpandPattern("/kubepods.slice/*", 2)
	if err != nil {
		t.Fatalf("ExpandPattern: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d paths, want 2", len(got))
	}
}

// The root cgroup holds machine-wide totals that belong to no cgroup in
// particular, and the per-group collector already refuses it. Refusing it here
// keeps the two paths consistent.
func TestExpandPatternRefusesRoot(t *testing.T) {
	r := newTreeReader(t)

	for _, pattern := range []string{"/", "", "//"} {
		if _, err := r.ExpandPattern(pattern, 0); err == nil {
			t.Errorf("pattern %q: expected an error", pattern)
		}
	}
}

// Expansion stays inside the root, like every other read on the Reader.
func TestExpandPatternRefusesEscape(t *testing.T) {
	r := newTreeReader(t)

	got, err := r.ExpandPattern("/../*", 0)
	if err == nil && len(got) > 0 {
		t.Errorf("got %v, want no paths outside the root", got)
	}
}
