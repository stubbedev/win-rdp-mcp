package main

import (
	"fmt"
	"image"
)

const pixelChangeThreshold = 24

type screenChange struct {
	inside, outside int
	area            int
}

func diffScreens(before, after *image.RGBA, area image.Rectangle) screenChange {
	area = area.Intersect(before.Bounds()).Intersect(after.Bounds())
	change := screenChange{area: area.Dx() * area.Dy()}
	b := before.Bounds().Intersect(after.Bounds())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			i := before.PixOffset(x, y)
			j := after.PixOffset(x, y)
			d := absDiff(before.Pix[i], after.Pix[j]) + absDiff(before.Pix[i+1], after.Pix[j+1]) + absDiff(before.Pix[i+2], after.Pix[j+2])
			if d <= pixelChangeThreshold {
				continue
			}
			if (image.Point{X: x, Y: y}).In(area) {
				change.inside++
			} else {
				change.outside++
			}
		}
	}
	return change
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

func runDialogArea(width, height int) image.Rectangle {
	return image.Rect(0, height-runDialogAreaHeight, min(width, runDialogAreaWidth), height)
}

const (
	runDialogAreaWidth  = 480
	runDialogAreaHeight = 340
)

func (c screenChange) dialogAppeared() bool {
	return c.area > 0 && c.inside*10 >= c.area*3 && c.outside*10 <= c.inside
}

func (c screenChange) landedInside() bool {
	return c.inside >= 150 && c.outside*2 <= c.inside
}

func (c screenChange) String() string {
	return fmt.Sprintf("%d of %d pixels changed in the Run corner, %d elsewhere", c.inside, c.area, c.outside)
}
