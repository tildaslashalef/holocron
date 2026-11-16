package provider

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// KeyFile is where a provider's key is read from (§5.5): the file a flag
// names, else the credential systemd delivered as <name>-key in
// $CREDENTIALS_DIRECTORY, else "" (no key: the provider is not used).
func KeyFile(name, flag string) string {
	if flag != "" {
		return flag
	}
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, name+"-key")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// ReadKey reads a key from path: one line, trimmed, not empty. A regular
// file that its group or others can read is refused, as ssh refuses a
// private key; a pipe (a shell's process substitution) is read as it is.
// The error never quotes the file's content.
func ReadKey(name, path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s: the key file: %w", name, err)
	}
	if fi.Mode().IsRegular() && fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s: %s is readable by others (mode %04o); chmod 600 it", name, path, fi.Mode().Perm())
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s: %s is a directory", name, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return "", fmt.Errorf("%s: %s cannot be read by this user", name, path)
		}
		return "", fmt.Errorf("%s: reading the key: %w", name, err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return "", fmt.Errorf("%s: %s does not hold one key on one line", name, path)
	}
	return key, nil
}
