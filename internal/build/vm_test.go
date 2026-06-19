package build

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCPUCores(t *testing.T) {
	cases := []struct {
		cpu  string
		want int64
	}{
		{"0", 1},     // defensive floor
		{"100m", 1},  // fractional rounds up to 1
		{"0.1", 1},   // same, decimal form
		{"500m", 1},  // still 1 core
		{"1", 1},     // exact
		{"1.5", 2},   // rounds up
		{"2", 2},     // exact
		{"2.5", 3},   // rounds up
		{"4", 4},     // exact
		{"4000m", 4}, // milli form
	}
	for _, c := range cases {
		got := cpuCores(resource.MustParse(c.cpu))
		if got != c.want {
			t.Errorf("cpuCores(%q) = %d, want %d", c.cpu, got, c.want)
		}
	}
}
