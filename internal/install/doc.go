// Package install sets a systemd host up for holocron (SPEC.md §9): its
// user, its keys as systemd credentials, its unit, from files embedded in
// the binary; and removes the SearXNG an older install left.
package install
