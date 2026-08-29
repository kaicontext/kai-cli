package main

import (
	"fmt"
	"testing"
)

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1023, "1023B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1048576, "1.0MB"},
		{1572864, "1.5MB"},
		{1073741824, "1.0GB"},
		{1610612736, "1.5GB"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.n), func(t *testing.T) {
			got := humanBytes(tc.n)
			if got != tc.want {
				t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}
