package rdpdr

import (
	"errors"
	"io/fs"
	"syscall"
)

type ntStatus uint32

const (
	statusSuccess              ntStatus = 0x00000000
	statusNoMoreFiles          ntStatus = 0x80000006
	statusUnsuccessful         ntStatus = 0xC0000001
	statusInvalidInfoClass     ntStatus = 0xC0000003
	statusInvalidHandle        ntStatus = 0xC0000008
	statusInvalidParameter     ntStatus = 0xC000000D
	statusNoSuchDevice         ntStatus = 0xC000000E
	statusNoSuchFile           ntStatus = 0xC000000F
	statusInvalidDeviceRequest ntStatus = 0xC0000010
	statusAccessDenied         ntStatus = 0xC0000022
	statusObjectNameCollision  ntStatus = 0xC0000035
	statusObjectPathNotFound   ntStatus = 0xC000003A
	statusDiskFull             ntStatus = 0xC000007F
	statusFileIsADirectory     ntStatus = 0xC00000BA
	statusNotSupported         ntStatus = 0xC00000BB
	statusDirectoryNotEmpty    ntStatus = 0xC0000101
	statusNotADirectory        ntStatus = 0xC0000103
)

func statusFor(err error) ntStatus {
	switch {
	case err == nil:
		return statusSuccess
	case errors.Is(err, fs.ErrNotExist):
		return statusNoSuchFile
	case errors.Is(err, fs.ErrExist):
		return statusObjectNameCollision
	case errors.Is(err, fs.ErrPermission):
		return statusAccessDenied
	case errors.Is(err, syscall.ENOTDIR):
		return statusNotADirectory
	case errors.Is(err, syscall.EISDIR):
		return statusFileIsADirectory
	case errors.Is(err, syscall.ENOTEMPTY):
		return statusDirectoryNotEmpty
	case errors.Is(err, syscall.ENOSPC):
		return statusDiskFull
	}
	return statusUnsuccessful
}
