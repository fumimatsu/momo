//go:build windows

package racerecorder

import "golang.org/x/sys/windows"

func availableBytes(path string) (int64, error) {
	var freeBytes uint64
	err := windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(path), &freeBytes, nil, nil)
	return int64(freeBytes), err
}
