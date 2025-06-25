//go:build !cdn
// +build !cdn

package static

// isCdnBuild returns false for standard builds
func isCdnBuild() bool {
	return false
}