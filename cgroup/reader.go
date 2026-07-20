package cgroup

import (
	"io"
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

// ReadUint64 reads and parses a single-value file (e.g. memory.current,
// pids.current) under the given cgroup path.
func (r *Reader) ReadUint64(cgroupPath, name string) (uint64, error) {
	content, err := r.readFile(cgroupPath, name)
	if err != nil {
		return 0, err
	}
	return ParseUint64(content)
}

// ReadKeyed reads and parses a "key value" file (e.g. memory.stat, cpu.stat)
// under the given cgroup path.
func (r *Reader) ReadKeyed(cgroupPath, name string) (map[string]uint64, error) {
	content, err := r.readFile(cgroupPath, name)
	if err != nil {
		return nil, err
	}
	return ParseKeyed(content)
}

// ReadPidsMax reads and parses pids.max under the given cgroup path, returning
// the limit and whether a finite limit is set ("max" => not limited).
func (r *Reader) ReadPidsMax(cgroupPath string) (limit uint64, limited bool, err error) {
	content, err := r.readFile(cgroupPath, "pids.max")
	if err != nil {
		return 0, false, err
	}
	return ParsePidsMax(content)
}

// ReadIOStat reads and parses io.stat (per-device "MAJ:MIN key=value ..." lines)
// under the given cgroup path.
func (r *Reader) ReadIOStat(cgroupPath string) (map[string]map[string]uint64, error) {
	content, err := r.readFile(cgroupPath, "io.stat")
	if err != nil {
		return nil, err
	}
	return ParseIOStat(content)
}
