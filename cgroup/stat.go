package cgroup

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// ParseUint64 parses a cgroupv2 single-value file such as memory.current or
// pids.current. The file holds a single line with one integer; some files may
// contain the literal "max" (e.g. pids.max), which callers of the *.current
// files won't see but which we map to 0 defensively.
func ParseUint64(content string) (uint64, error) {
	s := strings.TrimSpace(content)
	if s == "" {
		return 0, fmt.Errorf("empty value file")
	}
	if s == "max" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

// ParseKeyed parses a cgroupv2 "key value" file (one pair per line), such as
// memory.stat or cpu.stat, into a map. Values are unsigned integers. Lines that
// don't parse are skipped so a surprising field can't fail the whole read.
func ParseKeyed(content string) (map[string]uint64, error) {
	out := make(map[string]uint64)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		key, val, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		u, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if err != nil {
			continue
		}
		out[key] = u
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
