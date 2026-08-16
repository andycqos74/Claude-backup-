//go:build !linux

package agent

// isPseudoFS is Linux-specific: /proc and friends are a Linux concern, and
// the Windows and macOS equivalents are not reachable through an ordinary
// directory walk.
func isPseudoFS(string) bool { return false }
