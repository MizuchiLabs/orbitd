package updater

import (
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/assert"
)

func TestIsSelfDocker(t *testing.T) {
	u := &Updater{
		hostname: "abcdef123456",
	}

	tests := []struct {
		name     string
		c        container.Summary
		expected bool
	}{
		{
			name: "matches hostname",
			c: container.Summary{
				ID: "abcdef1234567890",
			},
			expected: true,
		},
		{
			name: "matches name",
			c: container.Summary{
				ID:    "1234567890abcdef",
				Names: []string{"/orbitd"},
			},
			expected: true,
		},
		{
			name: "no match",
			c: container.Summary{
				ID:    "1234567890abcdef",
				Names: []string{"/nginx"},
			},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, u.isSelfDocker(tc.c))
		})
	}
}
