package policy

import (
	"context"
	"testing"

	"github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/assert"
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
		{"unknown", Digest},
		{"", Digest},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.expected, Parse(tc.input))
		})
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
		image         string
		expectedRepo  string
		expectedTag   string
		expectedShort string
	}{
		{"nginx:1.21.1", "index.docker.io/library/nginx", "1.21.1", "nginx"},
		{"nginx", "index.docker.io/library/nginx", "latest", "nginx"},
		{"ghcr.io/mizuchilabs/orbitd:v0.1.0", "ghcr.io/mizuchilabs/orbitd", "v0.1.0", "orbitd"},
		{
			"mizuchilabs/orbitd@sha256:abcdef",
			"index.docker.io/mizuchilabs/orbitd",
			"latest",
			"orbitd",
		},
		{"nginx:1.21.1@sha256:abcdef", "index.docker.io/library/nginx", "1.21.1", "nginx"},
		{"myregistry.com:5000/myimage:v1.0", "myregistry.com:5000/myimage", "v1.0", "myimage"},
		{"myregistry.com/team/app:v1.2.3", "myregistry.com/team/app", "v1.2.3", "app"},
		{"bitnami/postgresql:14", "index.docker.io/bitnami/postgresql", "14", "postgresql"},
	}

	for _, tc := range tests {
		t.Run(tc.image, func(t *testing.T) {
			repo, tag, err := ParseImage(tc.image)
			assert.NoError(t, err)
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
	}

	for _, tc := range tests {
		t.Run(tc.v+"_"+tc.current+"_"+tc.policy.String(), func(t *testing.T) {
			v, _ := semver.NewVersion(tc.v)
			current, _ := semver.NewVersion(tc.current)
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
			assert.NoError(t, err)
			assert.Equal(t, tc.expected, target)
		})
	}
}

func TestFindBestVersion(t *testing.T) {
	repo := "nginx"
	tags := []string{"1.20.0", "1.21.0", "1.21.1", "1.22.0", "2.0.0"}

	tests := []struct {
		current  string
		policy   Policy
		expected string
	}{
		{"1.21.0", Patch, "nginx:1.21.1"},
		{"1.21.0", Minor, "nginx:1.22.0"},
		{"1.21.0", Major, "nginx:2.0.0"},
		{"2.0.0", Patch, "nginx:2.0.0"},
	}

	for _, tc := range tests {
		t.Run(tc.current+"_"+tc.policy.String(), func(t *testing.T) {
			current, _ := semver.NewVersion(tc.current)
			best, err := findBestVersion(repo, tags, current, tc.policy)
			assert.NoError(t, err)
			assert.Equal(t, tc.expected, best)
		})
	}
}
