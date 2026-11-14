package filetool

import (
	"fmt"
	"os"
	"syscall"
)

type fileIdentity struct{ volume, file uint64 }

func hardlinkIdentity(path string, info os.FileInfo) (fileIdentity, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return fileIdentity{}, false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return fileIdentity{}, false, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return fileIdentity{}, false, fmt.Errorf("file changed while opening")
	}
	var stat syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &stat); err != nil {
		return fileIdentity{}, false, err
	}
	identity := fileIdentity{uint64(stat.VolumeSerialNumber), uint64(stat.FileIndexHigh)<<32 | uint64(stat.FileIndexLow)}
	return identity, stat.NumberOfLinks > 1, nil
}
