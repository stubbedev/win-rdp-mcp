package rdpdr

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func diskSpace(dir string) (total, free uint64, err error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, fmt.Errorf("encoding %s: %w", dir, err)
	}
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, nil); err != nil {
		return 0, 0, fmt.Errorf("GetDiskFreeSpaceEx %s: %w", dir, err)
	}
	return total, free, nil
}
