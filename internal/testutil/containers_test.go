package testutil

import "testing"

func TestDockerRequired(t *testing.T) {
	tests := []struct {
		ci, require string
		want        bool
	}{
		{"", "", false},
		{"true", "", true},
		{"TRUE", "", true},
		{"false", "", false},
		{"", "1", true},
		{"", "0", false},
	}
	for _, tt := range tests {
		t.Setenv("CI", tt.ci)
		t.Setenv("REQUIRE_DOCKER", tt.require)
		if got := dockerRequired(); got != tt.want {
			t.Errorf("CI=%q REQUIRE_DOCKER=%q: got %v, want %v", tt.ci, tt.require, got, tt.want)
		}
	}
}
