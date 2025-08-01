package gl

import (
	"fmt"
)

const (
	// GardenLinuxRepo is the Git repository of GardenLinux.
	GardenLinuxRepo = "github.com/gardenlinux/gardenlinux"
)

// Manifest is a release manifest generated by the Garden Linux build system and possibly modified by GLCI.
//
//nolint:tagliatelle // Defined by the Garden Linux build system.
type Manifest struct {
	Version                string          `yaml:"version"`
	BuildCommittish        string          `yaml:"build_committish"`
	Architecture           Architecture    `yaml:"architecture"`
	Platform               string          `yaml:"platform"`
	Modifiers              []string        `yaml:"modifiers"`
	BuildTimestamp         string          `yaml:"build_timestamp"`
	Paths                  []S3ReleaseFile `yaml:"paths"`
	RequireUEFI            *bool           `yaml:"require_uefi,omitempty"`
	SecureBoot             *bool           `yaml:"secureboot,omitempty"`
	PublishedImageMetadata any             `yaml:"published_image_metadata"`
	S3Bucket               string          `yaml:"s3_bucket"`
}

// Architecture is a CPU architecture.
type Architecture string

const (
	// ArchitectureAMD64 stands for AMD64.
	ArchitectureAMD64 Architecture = "amd64"
	// ArchitectureARM64 stands for ARM64.
	ArchitectureARM64 Architecture = "arm64"
)

// S3ReleaseFile represents a file in S3 which is part of a release flavor.
//
//nolint:tagliatelle // Defined by the Garden Linux build system.
type S3ReleaseFile struct {
	Name         string  `yaml:"name"`
	Suffix       string  `yaml:"suffix"`
	MD5Sum       *string `yaml:"md5sum,omitempty"`
	SHA256Sum    *string `yaml:"sha256sum,omitempty"`
	S3Key        string  `yaml:"s3_key"`
	S3BucketName string  `yaml:"s3_bucket_name"`
}

// PathBySuffix resolves the file in S3 which has a given suffix.
func (m *Manifest) PathBySuffix(suffix string) (S3ReleaseFile, error) {
	for _, path := range m.Paths {
		if path.Suffix == suffix {
			return path, nil
		}
	}

	return S3ReleaseFile{}, fmt.Errorf("path for suffix %s missing in release manifest", suffix)
}
