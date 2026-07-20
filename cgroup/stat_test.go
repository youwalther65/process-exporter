package cgroup

import "testing"

func TestParseUint64(t *testing.T) {
	cases := []struct {
		in      string
		want    uint64
		wantErr bool
	}{
		{"104857600\n", 104857600, false},
		{"  42  ", 42, false},
		{"max\n", 0, false},
		{"", 0, true},
		{"notanumber", 0, true},
	}
	for _, c := range cases {
		got, err := ParseUint64(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseUint64(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("ParseUint64(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseKeyed(t *testing.T) {
	const memoryStat = `anon 1024
file 2048
kernel_stack 4096
slab 512
sock 256
`
	m, err := ParseKeyed(memoryStat)
	if err != nil {
		t.Fatalf("ParseKeyed: %v", err)
	}
	want := map[string]uint64{"anon": 1024, "file": 2048, "kernel_stack": 4096, "slab": 512, "sock": 256}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %d, want %d", k, m[k], v)
		}
	}
	if len(m) != len(want) {
		t.Errorf("got %d fields, want %d", len(m), len(want))
	}
}

func TestParseKeyed_SkipsMalformed(t *testing.T) {
	// A field with a non-numeric value or no space is skipped, not fatal.
	const cpuStat = `usage_usec 5000000
user_usec 3000000
system_usec notanumber
nr_periods 100
weirdlinewithnovalue
`
	m, err := ParseKeyed(cpuStat)
	if err != nil {
		t.Fatalf("ParseKeyed: %v", err)
	}
	if m["usage_usec"] != 5000000 || m["user_usec"] != 3000000 || m["nr_periods"] != 100 {
		t.Errorf("unexpected values: %+v", m)
	}
	if _, ok := m["system_usec"]; ok {
		t.Error("malformed system_usec should have been skipped")
	}
}

func TestParsePidsMax(t *testing.T) {
	cases := []struct {
		in          string
		wantLimit   uint64
		wantLimited bool
		wantErr     bool
	}{
		{"4096\n", 4096, true, false},
		{"  100  ", 100, true, false},
		{"max\n", 0, false, false},
		{"", 0, false, true},
		{"notanumber", 0, false, true},
	}
	for _, c := range cases {
		limit, limited, err := ParsePidsMax(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParsePidsMax(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && (limit != c.wantLimit || limited != c.wantLimited) {
			t.Errorf("ParsePidsMax(%q) = (%d,%v), want (%d,%v)", c.in, limit, limited, c.wantLimit, c.wantLimited)
		}
	}
}

func TestParseIOStat(t *testing.T) {
	// Two devices; the second is on an older kernel without discard fields. A
	// malformed pair (wios=abc) is skipped without failing the device.
	const ioStat = `259:0 rbytes=1048576 wbytes=2097152 rios=100 wios=200 dbytes=4096 dios=3
8:0 rbytes=512 wbytes=1024 rios=5 wios=abc
`
	m, err := ParseIOStat(ioStat)
	if err != nil {
		t.Fatalf("ParseIOStat: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("got %d devices, want 2", len(m))
	}
	if m["259:0"]["rbytes"] != 1048576 || m["259:0"]["wios"] != 200 || m["259:0"]["dios"] != 3 {
		t.Errorf("259:0 unexpected: %+v", m["259:0"])
	}
	if m["8:0"]["rios"] != 5 || m["8:0"]["rbytes"] != 512 {
		t.Errorf("8:0 unexpected: %+v", m["8:0"])
	}
	if _, ok := m["8:0"]["wios"]; ok {
		t.Error("malformed wios=abc should have been skipped")
	}
	if _, ok := m["8:0"]["dbytes"]; ok {
		t.Error("dbytes absent on older-kernel device should not appear")
	}
}

// TestParseIOStat_DeviceOnly verifies a device line with no counters still
// yields an (empty) entry rather than being dropped.
func TestParseIOStat_DeviceOnly(t *testing.T) {
	m, err := ParseIOStat("7:0\n")
	if err != nil {
		t.Fatalf("ParseIOStat: %v", err)
	}
	if _, ok := m["7:0"]; !ok {
		t.Errorf("expected device 7:0 present, got %+v", m)
	}
	if len(m["7:0"]) != 0 {
		t.Errorf("expected no counters for 7:0, got %+v", m["7:0"])
	}
}
