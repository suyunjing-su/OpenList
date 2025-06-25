//go:build cdn
// +build cdn

package static

// isCdnBuild returns true for CDN builds
func isCdnBuild() bool {
	return true
}