package main

import (
	"fmt"
	"strconv"
	"strings"
)

type keyDef struct {
	vk       uint16
	scan     uint8
	extended bool
}

var (
	keyCtrl  = keyDef{vk: 0x11, scan: 0x1D}
	keyAlt   = keyDef{vk: 0x12, scan: 0x38}
	keyShift = keyDef{vk: 0x10, scan: 0x2A}
	keyWin   = keyDef{vk: 0x5B, scan: 0x5B, extended: true}
	keyEnter = keyDef{vk: 0x0D, scan: 0x1C}
	keyTab   = keyDef{vk: 0x09, scan: 0x0F}
	keyA     = mustLookupKey("a")
	keyR     = mustLookupKey("r")
	keyV     = mustLookupKey("v")
	keyEsc   = mustLookupKey("esc")
	keyDel   = mustLookupKey("delete")
)

var namedKeys = map[string]keyDef{
	"ctrl": keyCtrl, "control": keyCtrl, "alt": keyAlt, "shift": keyShift,
	"win": keyWin, "super": keyWin, "cmd": keyWin, "meta": keyWin,
	"tab": keyTab, "enter": keyEnter, "return": keyEnter,
	"esc": {vk: 0x1B, scan: 0x01}, "escape": {vk: 0x1B, scan: 0x01},
	"space":     {vk: 0x20, scan: 0x39},
	"backspace": {vk: 0x08, scan: 0x0E},
	"delete":    {vk: 0x2E, scan: 0x53, extended: true}, "del": {vk: 0x2E, scan: 0x53, extended: true},
	"insert": {vk: 0x2D, scan: 0x52, extended: true}, "ins": {vk: 0x2D, scan: 0x52, extended: true},
	"home":        {vk: 0x24, scan: 0x47, extended: true},
	"end":         {vk: 0x23, scan: 0x4F, extended: true},
	"pageup":      {vk: 0x21, scan: 0x49, extended: true},
	"pagedown":    {vk: 0x22, scan: 0x51, extended: true},
	"up":          {vk: 0x26, scan: 0x48, extended: true},
	"down":        {vk: 0x28, scan: 0x50, extended: true},
	"left":        {vk: 0x25, scan: 0x4B, extended: true},
	"right":       {vk: 0x27, scan: 0x4D, extended: true},
	"capslock":    {vk: 0x14, scan: 0x3A},
	"printscreen": {vk: 0x2C, scan: 0x37, extended: true},
	"numlock":     {vk: 0x90, scan: 0x45},
	"volumemute":  {vk: 0xAD, scan: 0x20, extended: true},
	"volumedown":  {vk: 0xAE, scan: 0x2E, extended: true},
	"volumeup":    {vk: 0xAF, scan: 0x30, extended: true},
}

var characterKeys = func() map[rune]keyDef {
	m := map[rune]keyDef{
		'-': {vk: 0xBD, scan: 0x0C}, '=': {vk: 0xBB, scan: 0x0D},
		'[': {vk: 0xDB, scan: 0x1A}, ']': {vk: 0xDD, scan: 0x1B},
		'\\': {vk: 0xDC, scan: 0x2B}, ';': {vk: 0xBA, scan: 0x27},
		'\'': {vk: 0xDE, scan: 0x28}, '`': {vk: 0xC0, scan: 0x29},
		',': {vk: 0xBC, scan: 0x33}, '.': {vk: 0xBE, scan: 0x34},
		'/': {vk: 0xBF, scan: 0x35},
	}
	for i, r := range "1234567890" {
		m[r] = keyDef{vk: uint16(r), scan: uint8(0x02 + i)}
	}
	for _, row := range []struct {
		letters string
		first   uint8
	}{{"qwertyuiop", 0x10}, {"asdfghjkl", 0x1E}, {"zxcvbnm", 0x2C}} {
		for i, r := range row.letters {
			m[r] = keyDef{vk: uint16(r - 'a' + 'A'), scan: row.first + uint8(i)}
		}
	}
	return m
}()

var functionKeyScancodes = [24]uint8{
	0x3B, 0x3C, 0x3D, 0x3E, 0x3F, 0x40, 0x41, 0x42, 0x43, 0x44, 0x57, 0x58,
	0x64, 0x65, 0x66, 0x67, 0x68, 0x69, 0x6A, 0x6B, 0x6C, 0x6D, 0x6E, 0x76,
}

func lookupKey(name string) (keyDef, bool) {
	k := strings.ToLower(strings.TrimSpace(name))
	if def, ok := namedKeys[k]; ok {
		return def, true
	}
	if n, ok := functionKeyNumber(k); ok {
		return keyDef{vk: uint16(0x6F + n), scan: functionKeyScancodes[n-1]}, true
	}
	if r := []rune(k); len(r) == 1 {
		def, ok := characterKeys[r[0]]
		return def, ok
	}
	return keyDef{}, false
}

func mustLookupKey(name string) keyDef {
	def, ok := lookupKey(name)
	if !ok {
		panic("no key named " + name)
	}
	return def
}

func functionKeyNumber(k string) (int, bool) {
	digits, ok := strings.CutPrefix(k, "f")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 || n > 24 || strconv.Itoa(n) != digits {
		return 0, false
	}
	return n, true
}

func parseKeyCombo(raw string) ([]keyDef, error) {
	var keys []keyDef
	for part := range strings.SplitSeq(raw, "+") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		def, ok := lookupKey(name)
		if !ok {
			return nil, fmt.Errorf("unknown key %q", name)
		}
		keys = append(keys, def)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no keys given")
	}
	return keys, nil
}
