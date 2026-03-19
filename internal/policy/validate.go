package policy

import (
	"log/slog"
	"strings"
)

type Policy string

const (
	Digest Policy = "digest"
	Patch  Policy = "patch"
	Minor  Policy = "minor"
	Major  Policy = "major"
)

func (p Policy) String() string {
	return string(p)
}

func (p Policy) IsValid() bool {
	switch p {
	case Digest, Patch, Minor, Major:
		return true
	}
	return false
}

func Parse(pol string) Policy {
	p := normalize(pol)
	if p.IsValid() {
		return p
	}
	slog.Warn("Invalid policy", "policy", pol)
	return Digest
}

func ParseOr(pol string, fallback Policy) Policy {
	p := normalize(pol)
	if p.IsValid() {
		return p
	}
	slog.Warn("Invalid policy, using fallback", "policy", pol, "fallback", fallback)
	if fallback.IsValid() {
		return fallback
	}
	slog.Warn("Invalid fallback policy", "policy", fallback)
	return Digest
}

func normalize(p string) Policy {
	return Policy(strings.ToLower(strings.TrimSpace(p)))
}
