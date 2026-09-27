package updater

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mizuchilabs/orbitd/internal/policy"
)

func TestIsSelfDocker(t *testing.T) {
	u := &Updater{
		selfID: "abcdef123456",
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
			name: "name alone does not match",
			c: container.Summary{
				ID:    "1234567890abcdef",
				Names: []string{"/orbitd"},
			},
			expected: false,
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
			expected:     false,
		},
		{
			name:         "missing both image ids",
			currentImage: "",
			targetImage:  "",
			expected:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, isNewDockerImage(tc.currentImage, tc.targetImage))
		})
	}
}

func TestResolveTargetImageDigest(t *testing.T) {
	u := &Updater{Policy: policy.Digest}

	res, err := u.resolveTargetImage(context.Background(), nil, "nginx:1.25", nil)
	require.NoError(t, err)
	assert.Equal(t, "nginx:1.25", res.current)
	assert.Equal(t, "nginx:1.25", res.target)
	assert.Equal(t, policy.Digest, res.policy)

	// A pinned digest is dropped so the container follows the tag again.
	res, err = u.resolveTargetImage(context.Background(), nil, "nginx:1.25@sha256:abc", nil)
	require.NoError(t, err)
	assert.Equal(t, "nginx:1.25", res.target)
}

func TestResolveTargetImageLabelOverride(t *testing.T) {
	tests := []struct {
		name     string
		policy   policy.Policy
		label    map[string]string
		expected policy.Policy
	}{
		{
			name:     "label to digest overrides minor",
			policy:   policy.Minor,
			label:    map[string]string{"orbitd.policy": "digest"},
			expected: policy.Digest,
		},
		{
			name:     "invalid label falls back to default",
			policy:   policy.Digest,
			label:    map[string]string{"orbitd.policy": "garbage"},
			expected: policy.Digest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := &Updater{Policy: tc.policy}
			res, err := u.resolveTargetImage(context.Background(), nil, "nginx:1.25", tc.label)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, res.policy)
		})
	}
}

func TestPinImageDigest(t *testing.T) {
	assert.Equal(t,
		"nginx:1.25@sha256:abc",
		pinImageDigest("nginx:1.25", "sha256:abc"),
	)
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
		{
			name:         "missing target digest",
			currentRef:   "nginx:1.27@sha256:abc",
			targetDigest: "",
			expected:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, isNewSwarmImage(tc.currentRef, tc.targetDigest))
		})
	}
}

func TestContainerIDFromMountinfo(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	other := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

	inContainer := "" +
		"1 0 0:1 / / rw - overlay overlay rw\n" +
		"2 1 8:1 /var/lib/docker/containers/" + id + "/hostname /etc/hostname rw - ext4 /dev/sda1 rw\n"
	assert.Equal(t, id, containerIDFromMountinfo([]byte(inContainer)))

	// On the host, other containers' mounts show up but are not ours.
	onHost := "3 1 0:2 / /var/lib/docker/containers/" + other + "/mounts/shm rw - tmpfs shm rw\n"
	assert.Empty(t, containerIDFromMountinfo([]byte(onHost)))
}

func TestOnceMapRunsOncePerKey(t *testing.T) {
	var m onceMap[string]
	calls := 0
	fn := func() (string, error) { calls++; return "v", nil }

	for range 3 {
		v, err := m.do("a", fn)
		require.NoError(t, err)
		assert.Equal(t, "v", v)
	}
	_, _ = m.do("b", fn)
	assert.Equal(t, 2, calls)
}
