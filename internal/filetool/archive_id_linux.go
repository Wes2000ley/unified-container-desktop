package filetool

import (
	"fmt"
	"os"
	"syscall"
)

type fileIdentity struct{ volume, file uint64 }

func hardlinkIdentity(_ string, info os.FileInfo) (fileIdentity, bool, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, false, fmt.Errorf("filesystem did not provide file identity")
	}
	return fileIdentity{uint64(stat.Dev), stat.Ino}, stat.Nlink > 1, nil
}
