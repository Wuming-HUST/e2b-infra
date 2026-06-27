package sandbox

import (
	"strings"

	"github.com/Masterminds/semver/v3"
)

type VersionInfo struct {
	commitHash         string
	lastReleaseVersion semver.Version
}

func stripVersionPrefix(version string) string {
	return strings.TrimPrefix(version, "v")
}

func NewVersionInfo(fcVersion string) (info VersionInfo, err error) {
	// The structure of the fcVersion is last_tag[-prerelease]_commit_hash
	// Example: v1.0.0-release_1234567

	parts := strings.Split(fcVersion, "_")

	versionString := stripVersionPrefix(parts[0])

	version, versionErr := semver.NewVersion(versionString)
	if versionErr != nil {
		return info, versionErr
	}

	info.lastReleaseVersion = *version
	if len(parts) > 1 {
		info.commitHash = parts[1]
	} else {
		info.commitHash = ""
	}

	return info, nil
}

func (v *VersionInfo) Version() semver.Version {
	return v.lastReleaseVersion
}

func (v *VersionInfo) HasHugePages() bool {
	// Globally disabled: VMs use 4KB pages instead of 2MB huge pages.
	// Note: existing templates/snapshots built with 2MB huge pages cannot be
	// resumed under 4KB and must be rebuilt.
	return false
}
