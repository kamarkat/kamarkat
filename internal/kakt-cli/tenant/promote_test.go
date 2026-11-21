package tenant

import (
	"testing"
)

func TestContainsVersion(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		version  string
		expected bool
	}{
		{
			name:     "exact bare version",
			content:  "bundleVersion: 0.3.38\n",
			version:  "0.3.38",
			expected: true,
		},
		{
			name:     "quoted version",
			content:  `value: "0.3.38"`,
			version:  "0.3.38",
			expected: true,
		},
		{
			name:     "single quoted version",
			content:  `value: '0.3.38'`,
			version:  "0.3.38",
			expected: true,
		},
		{
			name:     "prerelease should not match base version",
			content:  "bundleVersion: 0.3.38-rc.1\n",
			version:  "0.3.38",
			expected: false,
		},
		{
			name:     "longer patch version should not match",
			content:  "version: 10.3.38",
			version:  "0.3.38",
			expected: false,
		},
		{
			name:     "prerelease exact match",
			content:  "bundleVersion: 0.3.38-rc.1\n",
			version:  "0.3.38-rc.1",
			expected: true,
		},
		{
			name:     "content contains prerelease and then exact version",
			content:  "old: 0.3.38-rc.1\nnew: 0.3.38\n",
			version:  "0.3.38",
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := containsVersion(tc.content, tc.version)
			if got != tc.expected {
				t.Errorf("containsVersion(%q, %q) = %v; want %v", tc.content, tc.version, got, tc.expected)
			}
		})
	}
}
