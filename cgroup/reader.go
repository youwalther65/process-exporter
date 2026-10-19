package cgroup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
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

// ErrTooManyMatches is returned by ExpandPattern when a pattern matches more
// cgroups than the caller allowed. The caller is expected to drop the pattern
// rather than emit the truncated set, because a partial expansion silently
// hides cgroups from the metrics.
var ErrTooManyMatches = errors.New("cgroup pattern matched more paths than the limit")

// childDirs lists the immediate child cgroups of cgroupPath. Only directories
// are returned, because in cgroupv2 a child cgroup is a directory and every
// other entry is an interface file.
func (r *Reader) childDirs(cgroupPath string) ([]string, error) {
	rel := strings.Trim(cgroupPath, "/")
	if rel == "" {
		rel = "."
	}
	f, err := r.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// ExpandPattern turns a cgroup path pattern into the concrete cgroup paths it
// matches, as absolute paths under the root ("/kubepods.slice/...").
//
// A segment containing a wildcard is matched against the child cgroups present
// at that level using path.Match, so "*" matches one level and never descends
// on its own. A segment with no wildcard is taken literally and is not listed,
// which keeps the common case of a fixed path at one syscall. Results are
// sorted, and each one is confirmed to be a directory, so a pattern that
// matches nothing returns an empty slice rather than paths that do not exist.
//
// max bounds the expansion at every level: a pattern that would exceed it
// returns ErrTooManyMatches with no paths. Per-pod cgroup names are unbounded
// in number, so this is the guard that keeps a NodeClass with many small pods
// from turning one pattern into thousands of series. max <= 0 means no limit.
func (r *Reader) ExpandPattern(pattern string, max int) ([]string, error) {
	segments := make([]string, 0, 8)
	for _, s := range strings.Split(pattern, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	// The root cgroup holds machine-wide totals that belong to no group, and
	// the group-labelled collector already refuses it. Refuse it here too so
	// the two paths agree.
	if len(segments) == 0 {
		return nil, fmt.Errorf("pattern %q resolves to the root cgroup", pattern)
	}

	paths := []string{""}
	for _, seg := range segments {
		if !strings.ContainsAny(seg, "*?[") {
			for i := range paths {
				paths[i] += "/" + seg
			}
			continue
		}
		next := make([]string, 0, len(paths))
		for _, p := range paths {
			children, err := r.childDirs(p)
			if err != nil {
				// A parent that has gone away mid-scrape is normal on a node
				// with churning pods, so it drops out instead of failing the
				// whole pattern.
				continue
			}
			for _, child := range children {
				ok, err := path.Match(seg, child)
				if err != nil {
					return nil, fmt.Errorf("pattern %q: %w", pattern, err)
				}
				if ok {
					next = append(next, p+"/"+child)
				}
			}
		}
		if max > 0 && len(next) > max {
			return nil, fmt.Errorf("%w: %q matched %d at segment %q, limit %d",
				ErrTooManyMatches, pattern, len(next), seg, max)
		}
		paths = next
	}

	out := make([]string, 0, len(paths))
	for _, p := range paths {
		fi, err := r.root.Stat(strings.TrimPrefix(p, "/"))
		if err != nil || !fi.IsDir() {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
