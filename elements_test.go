package main

import (
	"image"
	"strings"
	"testing"
)

func TestClickTarget(t *testing.T) {
	resolve := func(i int) (image.Point, error) { return image.Pt(i*10, i*20), nil }
	cases := []struct {
		args    arguments
		want    image.Point
		wantErr string
	}{
		{arguments{"x": 5.0, "y": 6.0}, image.Pt(5, 6), ""},
		{arguments{"element": 3.0}, image.Pt(30, 60), ""},
		{arguments{"element": 3.0, "x": 1.0}, image.Point{}, "not both"},
		{arguments{"x": 1.0}, image.Point{}, "give x and y"},
		{arguments{}, image.Point{}, "give x and y"},
	}
	for _, tc := range cases {
		got, err := clickTarget(tc.args, resolve)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("clickTarget(%v) error = %v, want %q", tc.args, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("clickTarget(%v) = %v, %v; want %v", tc.args, got, err, tc.want)
		}
	}
}

func TestElementCenterUsesTheLatestListing(t *testing.T) {
	rememberElements(nil)
	if _, err := elementCenter(1); err == nil || !strings.Contains(err.Error(), "Snapshot first") {
		t.Fatalf("lookup before any listing gave %v", err)
	}
	rememberElements([]uiElement{
		{Index: 1, Class: "Button", Text: "OK", Rect: image.Rect(10, 10, 30, 20)},
		{Index: 2, Class: "Edit", Rect: image.Rect(0, 100, 200, 120)},
	})
	if at, err := elementCenter(2); err != nil || at != image.Pt(100, 110) {
		t.Fatalf("element 2 = %v, %v", at, err)
	}
	if _, err := elementCenter(9); err == nil || !strings.Contains(err.Error(), "listed 2") {
		t.Fatalf("missing element gave %v", err)
	}
}
