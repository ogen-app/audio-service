package runtimetune

import (
	"io"
	"log/slog"
	"math"
	"runtime/debug"
	"testing"
	"testing/fstest"

	"github.com/ogen-app/audio-service/internal/config"
)

// discardLogger keeps the tuning logs out of test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCgroupMemoryLimit(t *testing.T) {
	const (
		v2Sentinel = "9223372036854771712" // cgroup v1/v2 "unlimited" page-count
	)
	tests := []struct {
		name   string
		files  fstest.MapFS
		want   int64
		wantOK bool
	}{
		{
			name:   "v2 finite",
			files:  fstest.MapFS{cgroupV2Max: {Data: []byte("536870912\n")}},
			want:   536870912,
			wantOK: true,
		},
		{
			name: "v2 max falls through to v1",
			files: fstest.MapFS{
				cgroupV2Max:   {Data: []byte("max\n")},
				cgroupV1Limit: {Data: []byte("268435456")},
			},
			want:   268435456,
			wantOK: true,
		},
		{
			name:   "v1 finite when v2 absent",
			files:  fstest.MapFS{cgroupV1Limit: {Data: []byte("134217728")}},
			want:   134217728,
			wantOK: true,
		},
		{
			name:   "v1 unlimited sentinel is not a limit",
			files:  fstest.MapFS{cgroupV1Limit: {Data: []byte(v2Sentinel)}},
			wantOK: false,
		},
		{
			name:   "empty file is not a limit",
			files:  fstest.MapFS{cgroupV2Max: {Data: []byte("  \n")}},
			wantOK: false,
		},
		{
			name:   "garbage is not a limit",
			files:  fstest.MapFS{cgroupV2Max: {Data: []byte("not-a-number")}},
			wantOK: false,
		},
		{
			name:   "no cgroup files at all",
			files:  fstest.MapFS{},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := cgroupMemoryLimit(tt.files)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("limit = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestApply_DerivesMemoryLimitFromCgroup(t *testing.T) {
	// debug.SetMemoryLimit is process-global; capture and restore it so this
	// test can't leak into others. A negative value reads without changing.
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	var cgroupLimit int64 = 1 << 30 // 1 GiB (runtime var so want isn't a const expr)
	root := fstest.MapFS{cgroupV2Max: {Data: []byte("1073741824")}}
	cfg := &config.Config{GCPercent: 0, MemoryLimitRatio: 0.9}

	// No GOMEMLIMIT in the env → the cgroup-derived path runs.
	apply(discardLogger(), cfg, root, func(string) (string, bool) { return "", false })

	want := int64(float64(cgroupLimit) * 0.9)
	if got := debug.SetMemoryLimit(-1); got != want {
		t.Errorf("GOMEMLIMIT = %d, want %d", got, want)
	}
}

func TestApply_RespectsExplicitGOMEMLIMIT(t *testing.T) {
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	root := fstest.MapFS{cgroupV2Max: {Data: []byte("1073741824")}}
	cfg := &config.Config{MemoryLimitRatio: 0.9}

	// GOMEMLIMIT set in the env → apply must not touch the runtime limit.
	apply(discardLogger(), cfg, root, func(k string) (string, bool) {
		if k == "GOMEMLIMIT" {
			return "256MiB", true
		}
		return "", false
	})

	if got := debug.SetMemoryLimit(-1); got != orig {
		t.Errorf("limit changed to %d despite explicit GOMEMLIMIT (was %d)", got, orig)
	}
}

func TestApply_RejectsInvalidRatios(t *testing.T) {
	// NaN/±Inf and finite ratios whose product overflows int64 must not reach
	// debug.SetMemoryLimit — the int64 conversion is implementation-defined for
	// them. A 1 GiB cgroup limit × 1e18 is finite but far past int64's ceiling.
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	root := fstest.MapFS{cgroupV2Max: {Data: []byte("1073741824")}} // 1 GiB
	noEnv := func(string) (string, bool) { return "", false }

	for _, tt := range []struct {
		name  string
		ratio float64
	}{
		{"NaN", math.NaN()},
		{"positive infinity", math.Inf(1)},
		{"overflowing finite", 1e18},
	} {
		t.Run(tt.name, func(t *testing.T) {
			debug.SetMemoryLimit(orig) // known baseline to assert against
			cfg := &config.Config{MemoryLimitRatio: tt.ratio}
			apply(discardLogger(), cfg, root, noEnv)
			if got := debug.SetMemoryLimit(-1); got != orig {
				t.Errorf("limit changed to %d for ratio %v; want unchanged %d", got, tt.ratio, orig)
			}
		})
	}
}

func TestApply_NoCgroupLeavesLimitUnchanged(t *testing.T) {
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	cfg := &config.Config{MemoryLimitRatio: 0.9}
	apply(discardLogger(), cfg, fstest.MapFS{}, func(string) (string, bool) { return "", false })

	if got := debug.SetMemoryLimit(-1); got != orig {
		t.Errorf("limit changed to %d with no cgroup (was %d)", got, orig)
	}
}
