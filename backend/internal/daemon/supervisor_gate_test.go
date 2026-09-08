package daemon

import "testing"

func TestShouldStartSupervisor(t *testing.T) {
	tests := []struct {
		name     string
		headless bool
		want     bool
	}{
		{"desktop default", false, true},
		{"headless service", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldStartSupervisor(tc.headless); got != tc.want {
				t.Errorf("shouldStartSupervisor(%v) = %v, want %v", tc.headless, got, tc.want)
			}
		})
	}
}
