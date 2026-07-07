package cgroup

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultRoot is the conventional mount point of the unified cgroupv2 hierarchy.
const DefaultRoot = "/sys/fs/cgroup"

// Reader reads cgroupv2 pseudo-files under a fixed cgroupfs root. The root is
// opened with os.Root so that all reads are confined to the cgroupfs subtree:
// a malicious or surprising symlink (or a ".." component) in a cgroup path
// cannot escape the root. This matters because the agent runs privileged and
// reads paths derived from /proc/<pid>/cgroup.
type Reader struct {
	root *os.Root
}

// NewReader opens rootPath (e.g. /sys/fs/cgroup) as an os.Root. The returned
// Reader confines every subsequent read to that subtree.
func NewReader(rootPath string) (*Reader, error) {
	r, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	return &Reader{root: r}, nil
}

// Close releases the underlying root handle.
func (r *Reader) Close() error {
	return r.root.Close()
}

// readFile reads a pseudo-file at cgroupPath/name relative to the root.
// cgroupPath is the cgroup's path as reported in /proc/<pid>/cgroup (e.g.
// "/runtime.slice/containerd.service"); leading and trailing slashes are
// trimmed so it joins cleanly under the root.
func (r *Reader) readFile(cgroupPath, name string) (string, error) {
	rel := filepath.Join(strings.Trim(cgroupPath, "/"), name)
	f, err := r.root.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()

	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadPSI reads and parses <cgroupPath>/<resource>.pressure, where resource is
// one of "cpu", "memory", or "io".
func (r *Reader) ReadPSI(cgroupPath, resource string) (PSIStats, error) {
	content, err := r.readFile(cgroupPath, resource+".pressure")
	if err != nil {
		return PSIStats{}, err
	}
	return ParsePSI(content)
}

// exists reports whether name exists under cgroupPath. Used to detect kernels
// or cgroups where PSI is not enabled (the *.pressure files are absent).
func (r *Reader) exists(cgroupPath, name string) bool {
	rel := filepath.Join(strings.Trim(cgroupPath, "/"), name)
	_, err := fs.Stat(r.root.FS(), rel)
	return err == nil
}
