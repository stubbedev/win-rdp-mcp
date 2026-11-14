package main

import (
	"unsafe"

	ole "github.com/go-ole/go-ole"
)

func variantArgs(v *ole.VARIANT) []uintptr {
	words := (*[4]uint32)(unsafe.Pointer(v))
	return []uintptr{uintptr(words[0]), uintptr(words[1]), uintptr(words[2]), uintptr(words[3])}
}
