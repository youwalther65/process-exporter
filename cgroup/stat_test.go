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
