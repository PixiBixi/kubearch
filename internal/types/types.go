// Package types defines shared OCI/container data types used across packages.
package types

// Platform represents a supported OS/architecture combination for a container image.
// Variant tells apart incompatible CPU generations of one arch (arm/v6, arm/v7).
type Platform struct {
	OS      string
	Arch    string
	Variant string
}
