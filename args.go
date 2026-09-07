package main

import (
	"strconv"
	"strings"
)

// arguments is one tool call's decoded arguments. Every accessor takes the
// value the caller asked for and a fallback, because JSON Schema defaults are
// documentation for the model, not something the SDK fills in.
type arguments map[string]any

func (a arguments) has(key string) bool {
	_, ok := a[key]
	return ok
}

func (a arguments) stringOr(key, fallback string) string {
	if s, ok := a[key].(string); ok && s != "" {
		return s
	}
	return fallback
}

// boolOr also accepts the strings "true"/"1"/"yes": some MCP clients send every
// argument as a string, and upstream tolerates that too.
func (a arguments) boolOr(key string, fallback bool) bool {
	switch v := a[key].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
	}
	return fallback
}

func (a arguments) floatOr(key string, fallback float64) float64 {
	if f, ok := toFloat(a[key]); ok {
		return f
	}
	return fallback
}

func (a arguments) intOr(key string, fallback int) int {
	if f, ok := toFloat(a[key]); ok {
		return int(f)
	}
	return fallback
}

func (a arguments) int64Or(key string, fallback int64) int64 {
	if f, ok := toFloat(a[key]); ok {
		return int64(f)
	}
	return fallback
}

// toFloat normalizes every numeric shape a JSON decoder may hand back —
// float64, the integer types, or a numeric string from a stringly-typed client.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}
