package crashlog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fatih/color"
)

// TestRenderGolden is the behavior-preservation oracle for the crashlog
// renderers: it pins the full rendered output of representative reports so a
// refactor that changes any byte of user-facing output fails loudly. Regenerate
// the fixtures with: go test ./pkg/crashlog -run TestRenderGolden -update
var updateGolden = os.Getenv("UPDATE_GOLDEN") != ""

func TestRenderGolden(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = prev })

	cases := []struct {
		golden string
		sample string
	}{
		{"298.golden", "JetsamEvent-2026-06-14-150819.ips"},
		{"309.golden", "Delta-2024-04-20-135807.ips"},
		{"309b.golden", "Contacts-2023-02-07-165803.ips"},
		{"309c.golden", "healthappd-2024-03-18-193212.ips"},
	}

	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			sample := filepath.Join("..", "..", "test-caches", "research", "crashlogs", tc.sample)
			if _, err := os.Stat(sample); err != nil {
				t.Skipf("sample not available: %v", err)
			}
			ips, err := OpenIPS(sample, &Config{})
			if err != nil {
				t.Fatalf("OpenIPS(%s): %v", tc.sample, err)
			}
			got := ips.String()

			goldenPath := filepath.Join("testdata", "golden", tc.golden)
			if updateGolden {
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatalf("update golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v", tc.golden, err)
			}
			if got != string(want) {
				t.Errorf("rendered output for %s drifted from golden %s; set UPDATE_GOLDEN=1 to refresh if intentional", tc.sample, tc.golden)
			}
		})
	}
}
