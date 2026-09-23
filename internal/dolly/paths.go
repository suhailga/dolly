package dolly

import (
	"os"
	"path/filepath"
)

// DataDir returns the path to ~/.dolly, creating it if absent.
func DataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".dolly")
	return dir, os.MkdirAll(dir, 0755)
}
