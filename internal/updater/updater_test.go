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

func TestShouldRecreateDocker(t *testing.T) {
	tests := []struct {
		name         string
		currentImage string
		targetImage  string
		expected     bool
	}{
		{
			name:         "same image id",
			currentImage: "sha256:current",
			targetImage:  "sha256:current",
			expected:     false,
		},
		{
			name:         "different image id",
			currentImage: "sha256:current",
			targetImage:  "sha256:next",
			expected:     true,
		},
		{
			name:         "missing current image id",
			currentImage: "",
			targetImage:  "sha256:next",
			expected:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, isNewDockerImage(tc.currentImage, tc.targetImage))
		})
	}
}

func TestShouldUpdateSwarm(t *testing.T) {
	tests := []struct {
		name         string
		currentRef   string
		targetDigest string
		expected     bool
	}{
		{
			name:         "same digest different formatting",
			currentRef:   "nginx:1.27@sha256:abc",
			targetDigest: "sha256:abc",
			expected:     false,
		},
		{
			name:         "different digest",
			currentRef:   "nginx:1.27@sha256:abc",
			targetDigest: "sha256:def",
			expected:     true,
		},
		{
			name:         "missing current digest",
			currentRef:   "nginx:1.27",
			targetDigest: "sha256:abc",
			expected:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, isNewSwarmImage(tc.currentRef, tc.targetDigest))
		})
	}
}
