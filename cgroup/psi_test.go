package cgroup

import "testing"

func TestParsePSI(t *testing.T) {
	const both = `some avg10=1.50 avg60=2.25 avg300=0.75 total=1234567
full avg10=0.10 avg60=0.20 avg300=0.30 total=76543
`
	stats, err := ParsePSI(both)
	if err != nil {
		t.Fatalf("ParsePSI: %v", err)
	}
	if stats.Some == nil || stats.Full == nil {
		t.Fatalf("expected both some and full, got %+v", stats)
	}
	if stats.Some.Avg10 != 1.50 || stats.Some.Avg60 != 2.25 || stats.Some.Avg300 != 0.75 {
		t.Errorf("some avgs wrong: %+v", stats.Some)
	}
	if stats.Some.Total != 1234567 {
		t.Errorf("some total = %d, want 1234567", stats.Some.Total)
	}
	if stats.Full.Total != 76543 {
		t.Errorf("full total = %d, want 76543", stats.Full.Total)
	}
}

func TestParsePSI_SomeOnly(t *testing.T) {
	// cpu.pressure on older kernels has no "full" line.
	stats, err := ParsePSI("some avg10=0.00 avg60=0.00 avg300=0.00 total=42\n")
	if err != nil {
		t.Fatalf("ParsePSI: %v", err)
	}
	if stats.Some == nil {
		t.Fatal("expected some line")
	}
	if stats.Full != nil {
		t.Errorf("expected no full line, got %+v", stats.Full)
	}
	if stats.Some.Total != 42 {
		t.Errorf("total = %d, want 42", stats.Some.Total)
	}
}

func TestParsePSI_UnknownKeyIgnored(t *testing.T) {
	// Tolerate future kernel additions.
	stats, err := ParsePSI("some avg10=0.00 avg60=0.00 avg300=0.00 total=7 newfield=99\n")
	if err != nil {
		t.Fatalf("ParsePSI should ignore unknown keys: %v", err)
	}
	if stats.Some.Total != 7 {
		t.Errorf("total = %d, want 7", stats.Some.Total)
	}
}

func TestParsePSI_Errors(t *testing.T) {
	cases := map[string]string{
		"bad kind":     "bogus avg10=0.00 total=1\n",
		"missing eq":   "some avg10 total=1\n",
		"bad total":    "some total=notanumber\n",
		"bad avg":      "some avg10=notafloat total=1\n",
		"too few flds": "some\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePSI(content); err == nil {
				t.Errorf("expected error for %q", content)
			}
		})
	}
}

func TestParsePSI_Empty(t *testing.T) {
	stats, err := ParsePSI("")
	if err != nil {
		t.Fatalf("empty content: %v", err)
	}
	if stats.Some != nil || stats.Full != nil {
		t.Errorf("expected zero stats, got %+v", stats)
	}
}
