// Package types defines shared OCI/container data types used across packages.
package types

// Platform represents a supported OS/architecture combination for a container image.
type Platform struct {
	OS   string
	Arch string
}
