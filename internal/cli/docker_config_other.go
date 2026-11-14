//go:build !windows

package cli

import "os"

func protectConfigCopy(source, target string) error {
	return os.Chmod(target, 0o600)
}

func replaceDockerConfig(source, target string, exists bool) error {
	return os.Rename(source, target)
}
