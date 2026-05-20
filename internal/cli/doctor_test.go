package cli

import "testing"

func TestParseJavaMajorVersion(t *testing.T) {
	cases := []struct {
		line string
		want int
	}{
		// Modern format: "openjdk 21.0.3 ..."
		{`openjdk 21.0.3 2024-04-16`, 21},
		{`openjdk 17.0.1 2021-10-19`, 17},
		{`openjdk 11.0.22 2024-01-16`, 11},
		// java prefix instead of openjdk
		{`java 21.0.3 2024-04-16`, 21},
		// Legacy 1.x format: "java version "1.8.0_292""
		{`java version "1.8.0_292"`, 8},
		{`java version "1.7.0_80"`, 7},
		// Version string with surrounding quotes (some JVM output styles)
		{`openjdk version "21.0.3" 2024-04-16`, 21},
		{`openjdk version "17.0.9" 2023-10-17`, 17},
		// Unrecognised / empty
		{``, 0},
		{`some random text`, 0},
		{`GraalVM CE 21.0.0`, 21},
	}
	for _, tc := range cases {
		got := parseJavaMajorVersion(tc.line)
		if got != tc.want {
			t.Errorf("parseJavaMajorVersion(%q) = %d, want %d", tc.line, got, tc.want)
		}
	}
}
