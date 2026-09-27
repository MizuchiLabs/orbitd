package policy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyIsValid(t *testing.T) {
	assert.True(t, Digest.IsValid())
	assert.True(t, Patch.IsValid())
	assert.True(t, Minor.IsValid())
	assert.True(t, Major.IsValid())
	assert.False(t, Policy("unknown").IsValid())
	assert.False(t, Policy("").IsValid())
}

func TestParse(t *testing.T) {
	tests := []struct {
		input    string
		expected Policy
	}{
		{"digest", Digest},
		{"patch", Patch},
		{"minor", Minor},
		{"major", Major},
		{"DIGEST", Digest},
		{"  patch  ", Patch},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			p, err := Parse(tc.input)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, p)
		})
	}

	for _, input := range []string{"unknown", ""} {
		_, err := Parse(input)
		assert.Error(t, err, input)
	}
}

func TestParseOr(t *testing.T) {
	tests := []struct {
		input    string
		fallback Policy
		expected Policy
	}{
		{"patch", Digest, Patch},
		{"unknown", Minor, Minor},
		{"", Major, Major},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.expected, ParseOr(tc.input, tc.fallback))
		})
	}
}

func TestParseImage(t *testing.T) {
	tests := []struct {
		image        string
		expectedRepo string
		expectedTag  string
	}{
		{"nginx:1.21.1", "nginx", "1.21.1"},
		{"nginx", "nginx", "latest"},
		{"ghcr.io/mizuchilabs/orbitd:v0.1.0", "ghcr.io/mizuchilabs/orbitd", "v0.1.0"},
		{"mizuchilabs/orbitd@sha256:abcdef", "mizuchilabs/orbitd", "latest"},
		{"nginx:1.21.1@sha256:abcdef", "nginx", "1.21.1"},
		{"myregistry.com:5000/myimage:v1.0", "myregistry.com:5000/myimage", "v1.0"},
		{"myregistry.com/team/app:v1.2.3", "myregistry.com/team/app", "v1.2.3"},
		{"bitnami/postgresql:14", "bitnami/postgresql", "14"},
	}

	for _, tc := range tests {
		t.Run(tc.image, func(t *testing.T) {
			repo, tag, err := ParseImage(tc.image)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedRepo, repo)
			assert.Equal(t, tc.expectedTag, tag)
		})
	}
}

func TestIsAllowed(t *testing.T) {
	tests := []struct {
		v        string
		current  string
		policy   Policy
		expected bool
	}{
		{"1.2.3", "1.2.2", Patch, true},
		{"1.2.4", "1.2.2", Patch, true},
		{"1.3.0", "1.2.2", Patch, false},
		{"2.0.0", "1.2.2", Patch, false},

		{"1.2.3", "1.2.2", Minor, true},
		{"1.3.0", "1.2.2", Minor, true},
		{"1.4.0", "1.2.2", Minor, true},
		{"2.0.0", "1.2.2", Minor, false},

		{"1.2.3", "1.2.2", Major, true},
		{"1.3.0", "1.2.2", Major, true},
		{"2.0.0", "1.2.2", Major, true},
		{"3.0.0", "1.2.2", Major, true},

		// No downgrades
		{"1.2.1", "1.2.2", Patch, false},
		{"1.1.0", "1.2.2", Minor, false},
		{"0.9.0", "1.2.2", Major, false},

		// Prereleases
		{"1.2.3-rc.1", "1.2.2", Patch, false},     // Don't move from stable to prerelease
		{"1.2.3", "1.2.3-rc.1", Patch, true},      // Move from prerelease to stable
		{"1.2.3-rc.2", "1.2.3-rc.1", Patch, true}, // Move between prereleases

		// Equal versions
		{"1.2.2", "1.2.2", Patch, false},
		{"1.2.2", "1.2.2", Minor, false},
		{"1.2.2", "1.2.2", Major, false},

		// Edge cases
		{
			"2.0.0-rc.1",
			"1.2.2",
			Major,
			false,
		}, // Don't move from stable to prerelease even for major
		{"2.0.0", "2.0.0-rc.1", Major, true}, // Move from prerelease to stable for major

		// Tag shape must match
		{"16.2-bookworm", "16.2-alpine", Patch, false}, // No variant switch
		{"16.4-alpine", "16.2-alpine", Minor, true},    // Same variant
		{"16.4", "16.2-alpine", Minor, false},          // Variant dropped
		{"1.25.3", "1.25", Patch, false},               // No floating -> pinned
		{"1.26", "1.25", Minor, true},                  // Floating minor stays floating
		{"20240101", "3", Major, false},                // Date tag is not a version bump
		{"10", "9", Major, true},                       // Normal width growth
		{"1.3.0", "v1.2.0", Minor, false},              // Prefix must match
		{"v1.3.0", "v1.2.0", Minor, true},
		{"1.2.4-alpine", "1.2.3-rc.1", Patch, false}, // Prerelease cannot switch variant
	}

	for _, tc := range tests {
		t.Run(tc.v+"_"+tc.current+"_"+tc.policy.String(), func(t *testing.T) {
			v, ok := parseVersion(tc.v)
			require.True(t, ok)
			current, ok := parseVersion(tc.current)
			require.True(t, ok)
			assert.Equal(t, tc.expected, isAllowed(v, current, tc.policy))
		})
	}
}

func TestFindUpdateTarget_EarlyExit(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		image    string
		policy   Policy
		expected string
	}{
		{"nginx:1.21.1", Digest, "nginx:1.21.1"},
		{"nginx@sha256:abcdef", Patch, "nginx@sha256:abcdef"},
		{"nginx:latest", Patch, "nginx:latest"}, // not semver
		{"nginx:not-semver", Patch, "nginx:not-semver"},
	}

	for _, tc := range tests {
		t.Run(tc.image+"_"+tc.policy.String(), func(t *testing.T) {
			target, err := FindUpdateTarget(ctx, tc.image, tc.policy)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, target)
		})
	}
}

func TestFindUpdateTargetInvalidImage(t *testing.T) {
	ctx := context.Background()

	_, err := FindUpdateTarget(ctx, "", Patch)
	require.Error(t, err)

	_, err = FindUpdateTarget(ctx, ":", Minor)
	require.Error(t, err)
}

func TestFindBestVersion(t *testing.T) {
	repo := "nginx"
	tags := []string{
		"1.20.0", "1.21.0", "1.21.1", "1.22.0", "2.0.0",
		"1.21.2-alpine", "1.23", "2.1.0-rc.1", "latest", "20240101",
	}

	tests := []struct {
		current  string
		policy   Policy
		expected string
	}{
		{"1.21.0", Patch, "nginx:1.21.1"},
		{"1.21.0", Minor, "nginx:1.22.0"},
		{"1.21.0", Major, "nginx:2.0.0"},
		{"2.0.0", Patch, "nginx:2.0.0"},
		{"1.21.0-alpine", Patch, "nginx:1.21.2-alpine"},
		{"1.21", Major, "nginx:1.23"},
	}

	for _, tc := range tests {
		t.Run(tc.current+"_"+tc.policy.String(), func(t *testing.T) {
			current, ok := parseVersion(tc.current)
			require.True(t, ok)
			best := findBestVersion(repo, tags, current, tc.policy)
			assert.Equal(t, tc.expected, best)
		})
	}
}
