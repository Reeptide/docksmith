package runtime

import (
	"strings"
	"testing"
)

func TestParseMemorySizeValid(t *testing.T) {
	cases := []struct {
		spec string
		want int64
	}{
		{"512m", 512 * 1024 * 1024},
		{"512M", 512 * 1024 * 1024},
		{"1g", 1 << 30},
		{"1G", 1 << 30},
		{"2048k", 2048 * 1024},
		{"2048K", 2048 * 1024},
		{"1073741824", 1073741824},
	}
	for _, c := range cases {
		got, err := ParseMemorySize(c.spec)
		if err != nil {
			t.Errorf("ParseMemorySize(%q) unexpected error: %v", c.spec, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMemorySize(%q) = %d, want %d", c.spec, got, c.want)
		}
	}
}

func TestParseMemorySizeRejectsBadSpecs(t *testing.T) {
	cases := map[string]string{
		"":            "empty",
		"-512m":       "negative",
		"0m":          "zero",
		"abc":         "non-numeric",
		"512x":        "unknown suffix",
		"512.5m":      "non-integer",
		"9999999999g": "overflows int64 bytes",
		"8589934593g": "just over int64 bytes max",
	}
	for spec, why := range cases {
		if _, err := ParseMemorySize(spec); err == nil {
			t.Errorf("ParseMemorySize(%q) should fail (%s)", spec, why)
		}
	}
}

func TestParseMemorySizeErrorNamesTheSpec(t *testing.T) {
	_, err := ParseMemorySize("512x")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "512x") {
		t.Errorf("error should quote the spec, got: %v", err)
	}
}

func TestParseCPUsValid(t *testing.T) {
	cases := []struct {
		spec string
		want float64
	}{
		{"1.5", 1.5},
		{"0.5", 0.5},
		{"4", 4},
		{"0.25", 0.25},
	}
	for _, c := range cases {
		got, err := ParseCPUs(c.spec)
		if err != nil {
			t.Errorf("ParseCPUs(%q) unexpected error: %v", c.spec, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseCPUs(%q) = %v, want %v", c.spec, got, c.want)
		}
	}
}

func TestParseCPUsRejectsBadSpecs(t *testing.T) {
	cases := map[string]string{
		"0":    "zero",
		"-1":   "negative",
		"abc":  "non-numeric",
		"":     "empty",
		"Inf":  "infinite",
		"-Inf": "negative infinite",
		"NaN":  "not a number",
		"1e15": "would overflow int64 in CPUMaxLine",
	}
	for spec, why := range cases {
		if _, err := ParseCPUs(spec); err == nil {
			t.Errorf("ParseCPUs(%q) should fail (%s)", spec, why)
		}
	}
}

func TestParsePidsLimitValid(t *testing.T) {
	cases := []struct {
		spec string
		want int64
	}{
		{"16", 16},
		{"32", 32},
		{"1000", 1000},
	}
	for _, c := range cases {
		got, err := ParsePidsLimit(c.spec)
		if err != nil {
			t.Errorf("ParsePidsLimit(%q) unexpected error: %v", c.spec, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParsePidsLimit(%q) = %d, want %d", c.spec, got, c.want)
		}
	}
}

// Values below MinPidsLimit are rejected rather than accepted: docksmith's
// own in-process init was observed crashing with a raw Go runtime panic
// ("failed to create new OS thread") when the cgroup pids limit was too low
// for its own supporting threads, well before the user's command ran.
func TestParsePidsLimitRejectsBadSpecs(t *testing.T) {
	cases := map[string]string{
		"0":   "zero",
		"-1":  "negative",
		"abc": "non-numeric",
		"":    "empty",
		"1":   "below MinPidsLimit, crashes docksmith's own init",
		"5":   "below MinPidsLimit, crashes docksmith's own init",
		"15":  "one under MinPidsLimit",
	}
	for spec, why := range cases {
		if _, err := ParsePidsLimit(spec); err == nil {
			t.Errorf("ParsePidsLimit(%q) should fail (%s)", spec, why)
		}
	}
}

func TestCPUMaxLine(t *testing.T) {
	cases := []struct {
		cpus float64
		want string
	}{
		{1.5, "150000 100000"},
		{0.25, "25000 100000"},
		{0, "max"},
		{-1, "max"},
	}
	for _, c := range cases {
		if got := CPUMaxLine(c.cpus); got != c.want {
			t.Errorf("CPUMaxLine(%v) = %q, want %q", c.cpus, got, c.want)
		}
	}
}

func TestPidsMaxLine(t *testing.T) {
	cases := []struct {
		limit int64
		want  string
	}{
		{32, "32"},
		{0, "max"},
		{-1, "max"},
	}
	for _, c := range cases {
		if got := PidsMaxLine(c.limit); got != c.want {
			t.Errorf("PidsMaxLine(%d) = %q, want %q", c.limit, got, c.want)
		}
	}
}

func TestMemoryMaxLine(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{67108864, "67108864"},
		{0, "max"},
		{-1, "max"},
	}
	for _, c := range cases {
		if got := MemoryMaxLine(c.bytes); got != c.want {
			t.Errorf("MemoryMaxLine(%d) = %q, want %q", c.bytes, got, c.want)
		}
	}
}

func TestResourceLimitsEmpty(t *testing.T) {
	var nilLimits *ResourceLimits
	if !nilLimits.Empty() {
		t.Error("nil ResourceLimits should be Empty")
	}
	if (&ResourceLimits{}).Empty() != true {
		t.Error("zero-value ResourceLimits should be Empty")
	}
	if (&ResourceLimits{MemoryBytes: 1}).Empty() {
		t.Error("MemoryBytes set should not be Empty")
	}
	if (&ResourceLimits{CPUQuota: 0.5}).Empty() {
		t.Error("CPUQuota set should not be Empty")
	}
	if (&ResourceLimits{PidsLimit: 1}).Empty() {
		t.Error("PidsLimit set should not be Empty")
	}
}
