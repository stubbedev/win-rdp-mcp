package main

import (
	"fmt"
	"image"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
)

var (
	clsidCUIAutomation = ole.NewGUID("{FF48DBA4-60EF-4201-AA87-54103EEF594E}")
	iidIUIAutomation   = ole.NewGUID("{30CBE57D-D9D0-452A-AB13-7AC5AC4825EE}")
)

const (
	uiaElementFromHandle        = 6
	uiaCreatePropertyCondition  = 23
	uiaElementFindAll           = 6
	uiaElementCurrentControlTyp = 21
	uiaElementCurrentName       = 23
	uiaElementCurrentIsEnabled  = 28
	uiaElementCurrentIsOffscrn  = 38
	uiaElementCurrentBoundRect  = 43
	uiaArrayLength              = 3
	uiaArrayGetElement          = 4

	uiaTreeScopeDescendants           = 4
	uiaIsKeyboardFocusableProp        = 30009
	maxAutomationElements             = 200
	variantTrue                uint16 = 0xFFFF
)

var controlTypeNames = map[int32]string{
	50000: "Button", 50001: "Calendar", 50002: "CheckBox", 50003: "ComboBox", 50004: "Edit",
	50005: "Hyperlink", 50006: "Image", 50007: "ListItem", 50008: "List", 50009: "Menu",
	50010: "MenuBar", 50011: "MenuItem", 50012: "ProgressBar", 50013: "RadioButton",
	50014: "ScrollBar", 50015: "Slider", 50016: "Spinner", 50017: "StatusBar", 50018: "Tab",
	50019: "TabItem", 50020: "Text", 50021: "ToolBar", 50022: "ToolTip", 50023: "Tree",
	50024: "TreeItem", 50025: "Custom", 50026: "Group", 50027: "Thumb", 50028: "DataGrid",
	50029: "DataItem", 50030: "Document", 50031: "SplitButton", 50032: "Window", 50033: "Pane",
	50034: "Header", 50035: "HeaderItem", 50036: "Table", 50037: "TitleBar", 50038: "Separator",
	50039: "SemanticZoom", 50040: "AppBar",
}

type comObject struct{ ptr uintptr }

func (o comObject) call(index int, args ...uintptr) error {
	vtbl := *(*uintptr)(unsafe.Pointer(o.ptr))
	fn := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(index)*unsafe.Sizeof(uintptr(0))))
	hr, _, _ := syscall.SyscallN(fn, append([]uintptr{o.ptr}, args...)...)
	if int32(hr) < 0 {
		return ole.NewError(hr)
	}
	return nil
}

func (o comObject) release() {
	if o.ptr != 0 {
		(*ole.IUnknown)(unsafe.Pointer(o.ptr)).Release()
	}
}

func (o comObject) object(index int, args ...uintptr) (comObject, error) {
	var out uintptr
	if err := o.call(index, append(args, uintptr(unsafe.Pointer(&out)))...); err != nil {
		return comObject{}, err
	}
	if out == 0 {
		return comObject{}, fmt.Errorf("UI Automation returned no object from method %d", index)
	}
	return comObject{out}, nil
}

func (o comObject) int32Value(index int) (int32, error) {
	var v int32
	err := o.call(index, uintptr(unsafe.Pointer(&v)))
	return v, err
}

func (o comObject) stringValue(index int) (string, error) {
	var bstr *uint16
	if err := o.call(index, uintptr(unsafe.Pointer(&bstr))); err != nil {
		return "", err
	}
	if bstr == nil {
		return "", nil
	}
	defer ole.SysFreeString((*int16)(unsafe.Pointer(bstr)))
	return ole.BstrToString(bstr), nil
}

func automationElements(hwnd uintptr) ([]uiElement, error) {
	var out []uiElement
	err := withCOM(func() error {
		unknown, err := ole.CreateInstance(clsidCUIAutomation, iidIUIAutomation)
		if err != nil {
			return fmt.Errorf("creating the UI Automation client: %w", err)
		}
		automation := comObject{uintptr(unsafe.Pointer(unknown))}
		defer automation.release()

		root, err := automation.object(uiaElementFromHandle, hwnd)
		if err != nil {
			return fmt.Errorf("reading the foreground window's UI Automation element: %w", err)
		}
		defer root.release()

		focusable := ole.VARIANT{VT: ole.VT_BOOL, Val: int64(variantTrue)}
		condition, err := automation.object(uiaCreatePropertyCondition, append([]uintptr{uiaIsKeyboardFocusableProp}, variantArgs(&focusable)...)...)
		if err != nil {
			return fmt.Errorf("building the UI Automation condition: %w", err)
		}
		defer condition.release()

		found, err := root.object(uiaElementFindAll, uiaTreeScopeDescendants, condition.ptr)
		if err != nil {
			return fmt.Errorf("finding interactive elements: %w", err)
		}
		defer found.release()

		count, err := found.int32Value(uiaArrayLength)
		if err != nil {
			return err
		}
		for i := int32(0); i < count && len(out) < maxAutomationElements; i++ {
			el, err := found.object(uiaArrayGetElement, uintptr(i))
			if err != nil {
				return err
			}
			item, keep := describeElement(el)
			el.release()
			if keep {
				item.Index = len(out) + 1
				out = append(out, item)
			}
		}
		return nil
	})
	return out, err
}

func describeElement(el comObject) (uiElement, bool) {
	if enabled, err := el.int32Value(uiaElementCurrentIsEnabled); err != nil || enabled == 0 {
		return uiElement{}, false
	}
	if offscreen, err := el.int32Value(uiaElementCurrentIsOffscrn); err != nil || offscreen != 0 {
		return uiElement{}, false
	}
	var r rect
	if err := el.call(uiaElementCurrentBoundRect, uintptr(unsafe.Pointer(&r))); err != nil {
		return uiElement{}, false
	}
	bounds := image.Rect(int(r.Left), int(r.Top), int(r.Right), int(r.Bottom))
	if bounds.Empty() {
		return uiElement{}, false
	}
	controlType, err := el.int32Value(uiaElementCurrentControlTyp)
	if err != nil {
		return uiElement{}, false
	}
	kind, ok := controlTypeNames[controlType]
	if !ok {
		kind = fmt.Sprintf("Control %d", controlType)
	}
	name, err := el.stringValue(uiaElementCurrentName)
	if err != nil {
		return uiElement{}, false
	}
	return uiElement{Class: kind, Text: name, Rect: bounds}, true
}
