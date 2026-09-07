//go:build windows

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

var registryRoots = map[string]registry.Key{
	"HKCR": registry.CLASSES_ROOT, "HKEY_CLASSES_ROOT": registry.CLASSES_ROOT,
	"HKCU": registry.CURRENT_USER, "HKEY_CURRENT_USER": registry.CURRENT_USER,
	"HKLM": registry.LOCAL_MACHINE, "HKEY_LOCAL_MACHINE": registry.LOCAL_MACHINE,
	"HKU": registry.USERS, "HKEY_USERS": registry.USERS,
	"HKCC": registry.CURRENT_CONFIG, "HKEY_CURRENT_CONFIG": registry.CURRENT_CONFIG,
}

// parseRegistryKey splits "HKLM\SOFTWARE\Foo" into its root handle and the
// subkey path beneath it.
func parseRegistryKey(key string) (registry.Key, string, error) {
	rootName, subKey, _ := strings.Cut(strings.TrimSpace(key), `\`)
	root, ok := registryRoots[strings.ToUpper(rootName)]
	if !ok {
		return 0, "", fmt.Errorf("unknown root key %q (use HKCR, HKCU, HKLM, HKU or HKCC)", rootName)
	}
	return root, subKey, nil
}

func regReadTool(_ context.Context, args arguments) (toolResult, error) {
	root, subKey, err := parseRegistryKey(args.stringOr("key", ""))
	if err != nil {
		return toolResult{}, err
	}
	k, err := registry.OpenKey(root, subKey, registry.QUERY_VALUE)
	if err != nil {
		return toolResult{}, fmt.Errorf("opening %s: %w", args.stringOr("key", ""), err)
	}
	defer k.Close()

	name := args.stringOr("value_name", "")
	// A zero-length read reports the value's type without fetching it, which is
	// what decides how to render it below.
	_, valueType, err := k.GetValue(name, nil)
	if err != nil {
		return toolResult{}, fmt.Errorf("reading %s: %w", name, err)
	}

	switch valueType {
	case registry.SZ, registry.EXPAND_SZ:
		s, _, err := k.GetStringValue(name)
		if err != nil {
			return toolResult{}, err
		}
		return textResult("%s (%s)", s, regTypeName(valueType)), nil
	case registry.DWORD, registry.QWORD:
		n, _, err := k.GetIntegerValue(name)
		if err != nil {
			return toolResult{}, err
		}
		return textResult("%d (%s)", n, regTypeName(valueType)), nil
	case registry.MULTI_SZ:
		values, _, err := k.GetStringsValue(name)
		if err != nil {
			return toolResult{}, err
		}
		return textResult("%s (REG_MULTI_SZ)", strings.Join(values, "|")), nil
	case registry.BINARY:
		raw, _, err := k.GetBinaryValue(name)
		if err != nil {
			return toolResult{}, err
		}
		return textResult("%s (REG_BINARY, %d bytes)", hex.EncodeToString(raw), len(raw)), nil
	}
	return toolResult{}, fmt.Errorf("unsupported registry value type %d", valueType)
}

func regTypeName(t uint32) string {
	switch t {
	case registry.SZ:
		return "REG_SZ"
	case registry.EXPAND_SZ:
		return "REG_EXPAND_SZ"
	case registry.DWORD:
		return "REG_DWORD"
	case registry.QWORD:
		return "REG_QWORD"
	case registry.MULTI_SZ:
		return "REG_MULTI_SZ"
	case registry.BINARY:
		return "REG_BINARY"
	}
	return "type " + strconv.FormatUint(uint64(t), 10)
}

func regWriteTool(_ context.Context, args arguments) (toolResult, error) {
	keyPath := args.stringOr("key", "")
	root, subKey, err := parseRegistryKey(keyPath)
	if err != nil {
		return toolResult{}, err
	}
	name := args.stringOr("value_name", "")
	data := args.stringOr("data", "")

	k, _, err := registry.CreateKey(root, subKey, registry.SET_VALUE)
	if err != nil {
		return toolResult{}, fmt.Errorf("opening %s for write: %w", keyPath, err)
	}
	defer k.Close()

	regType := strings.ToUpper(args.stringOr("reg_type", "REG_SZ"))
	switch regType {
	case "REG_SZ":
		err = k.SetStringValue(name, data)
	case "REG_EXPAND_SZ":
		err = k.SetExpandStringValue(name, data)
	case "REG_DWORD":
		var n uint64
		if n, err = strconv.ParseUint(data, 0, 32); err == nil {
			err = k.SetDWordValue(name, uint32(n))
		}
	case "REG_QWORD":
		var n uint64
		if n, err = strconv.ParseUint(data, 0, 64); err == nil {
			err = k.SetQWordValue(name, n)
		}
	case "REG_MULTI_SZ":
		err = k.SetStringsValue(name, strings.Split(data, "|"))
	case "REG_BINARY":
		var raw []byte
		if raw, err = hex.DecodeString(strings.TrimPrefix(data, "0x")); err == nil {
			err = k.SetBinaryValue(name, raw)
		}
	default:
		return toolResult{}, fmt.Errorf("unknown registry type %q", regType)
	}
	if err != nil {
		return toolResult{}, fmt.Errorf("writing %s: %w", name, err)
	}
	return textResult("Wrote %s = %q (%s) to %s", name, data, regType, keyPath), nil
}
