//go:build windows && (amd64 || arm64)

package main

import (
	"unsafe"

	ole "github.com/go-ole/go-ole"
)

func variantArgs(v *ole.VARIANT) []uintptr { return []uintptr{uintptr(unsafe.Pointer(v))} }
