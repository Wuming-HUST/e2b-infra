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

// HasHugePages reports whether 2MB huge pages should be used. It is gated both on
// the caller-provided config toggle (enabled) and on the Firecracker version
// supporting huge pages. When disabled, VMs use 4KB pages instead.
//
// Note: templates/snapshots built with a different page size cannot be resumed
// and must be rebuilt after toggling this.
func (v *VersionInfo) HasHugePages(enabled bool) bool {
	if !enabled {
		return false
	}

	return v.lastReleaseVersion.Major() >= 1 && v.lastReleaseVersion.Minor() >= 7
}
