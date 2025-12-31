//go:build linux

package resources

import "testing"

func TestParseCPUSet(t *testing.T) {
	tests := []struct {
		name     string
		cpuset   string
		expected int
	}{
		// Valid formats
		{
			name:     "simple range",
			cpuset:   "0-3",
			expected: 4,
		},
		{
			name:     "single cpu",
			cpuset:   "5",
			expected: 1,
		},
		{
			name:     "list of cpus",
			cpuset:   "0,2,4,6",
			expected: 4,
		},
		{
			name:     "mixed ranges and lists",
			cpuset:   "0-3,8-11",
			expected: 8,
		},
		{
			name:     "complex mixed",
			cpuset:   "0-1,4,6-7",
			expected: 5,
		},
		{
			name:     "whitespace handling",
			cpuset:   " 0-3 , 8-11 ",
			expected: 8,
		},
		{
			name:     "duplicate cpus",
			cpuset:   "0,1,1,2,2,3",
			expected: 4, // Should deduplicate
		},
		{
			name:     "overlapping ranges",
			cpuset:   "0-5,3-7",
			expected: 8, // 0,1,2,3,4,5,6,7
		},

		// Invalid formats - should return 0
		{
			name:     "empty string",
			cpuset:   "",
			expected: 0,
		},
		{
			name:     "only whitespace",
			cpuset:   "   ",
			expected: 0,
		},
		{
			name:     "invalid range (end < start)",
			cpuset:   "5-2",
			expected: 0,
		},
		{
			name:     "negative numbers",
			cpuset:   "-1-5",
			expected: 0,
		},
		{
			name:     "invalid characters",
			cpuset:   "0-3,abc",
			expected: 0,
		},
		{
			name:     "incomplete range",
			cpuset:   "0-",
			expected: 0,
		},
		{
			name:     "multiple dashes",
			cpuset:   "0-3-5",
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseCPUSet(tt.cpuset)
			if result != tt.expected {
				t.Errorf("parseCPUSet(%q) = %d, want %d", tt.cpuset, result, tt.expected)
			}
		})
	}
}
