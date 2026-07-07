// Package cgroup reads cgroupv2 pressure (PSI) and stat files for a resolved
// cgroup path. It is deliberately independent of the /proc reading in package
// proc: given a cgroupfs root and a cgroup path, it reads and parses the
// pseudo-files the collector is configured to export.
//
// All cgroupv2 pseudo-files read here (cpu.pressure, memory.pressure,
// io.pressure, memory.stat, ...) are world-readable, so no elevated privileges
// are required to read them; a privileged DaemonSet is only needed so the agent
// can see cgroups it does not own.
package cgroup

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// PSILine holds one line of a pressure file, e.g.
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=12345
//
// Avg10/Avg60/Avg300 are percentages (0-100) as reported by the kernel.
// Total is the accumulated stall time in microseconds.
type PSILine struct {
	Avg10  float64
	Avg60  float64
	Avg300 float64
	Total  uint64
}

// PSIStats holds the "some" and "full" lines of a pressure file. cpu.pressure
// on older kernels has no "full" line, so Full may be nil.
type PSIStats struct {
	Some *PSILine
	Full *PSILine
}

// ParsePSI parses the contents of a cgroupv2 *.pressure file.
func ParsePSI(content string) (PSIStats, error) {
	var stats PSIStats
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		kind, psi, err := parsePSILine(line)
		if err != nil {
			return PSIStats{}, err
		}
		switch kind {
		case "some":
			stats.Some = psi
		case "full":
			stats.Full = psi
		default:
			return PSIStats{}, fmt.Errorf("unknown PSI line kind %q", kind)
		}
	}
	if err := scanner.Err(); err != nil {
		return PSIStats{}, err
	}
	return stats, nil
}

// parsePSILine parses a single line like
// "some avg10=0.00 avg60=0.00 avg300=0.00 total=12345", returning the kind
// ("some" or "full") and the parsed values.
func parsePSILine(line string) (string, *PSILine, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", nil, fmt.Errorf("malformed PSI line %q", line)
	}
	kind := fields[0]
	psi := &PSILine{}
	for _, kv := range fields[1:] {
		key, val, found := strings.Cut(kv, "=")
		if !found {
			return "", nil, fmt.Errorf("malformed PSI field %q in line %q", kv, line)
		}
		switch key {
		case "avg10":
			f, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return "", nil, fmt.Errorf("bad avg10 %q: %v", val, err)
			}
			psi.Avg10 = f
		case "avg60":
			f, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return "", nil, fmt.Errorf("bad avg60 %q: %v", val, err)
			}
			psi.Avg60 = f
		case "avg300":
			f, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return "", nil, fmt.Errorf("bad avg300 %q: %v", val, err)
			}
			psi.Avg300 = f
		case "total":
			u, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				return "", nil, fmt.Errorf("bad total %q: %v", val, err)
			}
			psi.Total = u
		default:
			// Ignore unknown keys so we tolerate future kernel additions.
		}
	}
	return kind, psi, nil
}
