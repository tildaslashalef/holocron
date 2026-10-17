// Package install sets a systemd host up for holocron (SPEC.md §9):
// its user, SearXNG as a Podman quadlet, its own unit, from files
// embedded in the binary.
package install

// The SearXNG image this release installs, pinned by digest (§9.1).
const (
	SearXNGImage = "docker.io/searxng/searxng@sha256:ad273460b12390ab1ad9c6ea630dafed39045ab0b9096a85ca31f5246517c670"
	SearXNGTag   = "2026.10.15-b00212a83"
)
