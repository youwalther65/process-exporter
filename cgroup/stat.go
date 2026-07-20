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

// ParsePidsMax parses pids.max, which holds either an integer limit or the
// literal "max" meaning no limit. It returns the limit and whether a finite
// limit is set; the unlimited case returns (0, false, nil). This is distinct
// from ParseUint64 (which maps "max" to 0) because for a *limit* 0 and
// "unlimited" are opposites, and conflating them would make a saturation ratio
// meaningless.
func ParsePidsMax(content string) (limit uint64, limited bool, err error) {
	s := strings.TrimSpace(content)
	if s == "" {
		return 0, false, fmt.Errorf("empty value file")
	}
	if s == "max" {
		return 0, false, nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
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

// ParseIOStat parses a cgroupv2 io.stat file into a per-device map of counters.
// Each line has the form
//
//	MAJ:MIN rbytes=<n> wbytes=<n> rios=<n> wios=<n> dbytes=<n> dios=<n>
//
// where the "MAJ:MIN" device identifier is the map key and the remaining
// "key=value" pairs become that device's counters. The set of fields varies by
// kernel (dbytes/dios were added later), so unknown or unparseable fields are
// skipped rather than failing the whole read, matching ParseKeyed. A device
// line with no parseable fields still yields an (empty) entry so callers can see
// the device was present.
func ParseIOStat(content string) (map[string]map[string]uint64, error) {
	out := make(map[string]map[string]uint64)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		// The device identifier is the first field; a line with only the
		// device and no counters is unusual but harmless.
		dev := fields[0]
		counters := make(map[string]uint64)
		for _, pair := range fields[1:] {
			key, val, found := strings.Cut(pair, "=")
			if !found {
				continue
			}
			u, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				continue
			}
			counters[key] = u
		}
		out[dev] = counters
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
