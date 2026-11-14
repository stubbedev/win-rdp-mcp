package main

import (
	"errors"
	"fmt"
	"image"
	"sync"
)

var listedElements struct {
	sync.Mutex
	list []uiElement
}

func rememberElements(elements []uiElement) {
	listedElements.Lock()
	defer listedElements.Unlock()
	listedElements.list = elements
}

func elementCenter(index int) (image.Point, error) {
	listedElements.Lock()
	defer listedElements.Unlock()
	if len(listedElements.list) == 0 {
		return image.Point{}, errors.New("no elements have been listed yet; take a Snapshot first")
	}
	for _, el := range listedElements.list {
		if el.Index == index {
			x, y := el.center()
			return image.Pt(x, y), nil
		}
	}
	return image.Point{}, fmt.Errorf("element %d is not in the latest Snapshot, which listed %d elements", index, len(listedElements.list))
}

func clickTarget(args arguments, resolve func(int) (image.Point, error)) (image.Point, error) {
	if args.has("element") {
		if args.has("x") || args.has("y") {
			return image.Point{}, errors.New("give either element or x and y, not both")
		}
		return resolve(args.intOr("element", 0))
	}
	if !args.has("x") || !args.has("y") {
		return image.Point{}, errors.New("give x and y, or an element number from the latest Snapshot")
	}
	return image.Pt(args.intOr("x", 0), args.intOr("y", 0)), nil
}
