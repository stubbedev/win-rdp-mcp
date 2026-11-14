package main

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"time"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

const (
	taskCreateOrUpdate        = 6
	taskLogonInteractiveToken = 3
	taskActionExec            = 0
	taskEnumHidden            = 1
	allMonths                 = 0xFFF
	taskTimeLayout            = "2006-01-02T15:04:05Z"
)

var comTriggerTypes = map[taskTrigger]int{
	triggerOnce: 1, triggerDaily: 2, triggerWeekly: 3, triggerMonthly: 4,
	triggerOnIdle: 6, triggerOnStart: 8, triggerOnLogon: 9,
}

var taskStates = map[int64]string{0: "Unknown", 1: "Disabled", 2: "Queued", 3: "Ready", 4: "Running"}

func withCOM(f func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		var oleErr *ole.OleError
		if !errors.As(err, &oleErr) || oleErr.Code() != 1 {
			return fmt.Errorf("initialising COM: %w", err)
		}
	}
	defer ole.CoUninitialize()
	return f()
}

func dispatchResult(v *ole.VARIANT, err error, what string) (*ole.IDispatch, error) {
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	disp := v.ToIDispatch()
	if disp == nil {
		v.Clear()
		return nil, fmt.Errorf("%s returned no object", what)
	}
	return disp, nil
}

func callObject(d *ole.IDispatch, method string, args ...any) (*ole.IDispatch, error) {
	v, err := oleutil.CallMethod(d, method, args...)
	return dispatchResult(v, err, method)
}

func getObject(d *ole.IDispatch, prop string) (*ole.IDispatch, error) {
	v, err := oleutil.GetProperty(d, prop)
	return dispatchResult(v, err, prop)
}

func putProperty(d *ole.IDispatch, prop string, value any) error {
	v, err := oleutil.PutProperty(d, prop, value)
	if err != nil {
		return fmt.Errorf("setting %s: %w", prop, err)
	}
	return v.Clear()
}

func getValue(d *ole.IDispatch, prop string) (any, error) {
	v, err := oleutil.GetProperty(d, prop)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", prop, err)
	}
	defer v.Clear()
	return v.Value(), nil
}

func withTaskService(f func(service, root *ole.IDispatch) error) error {
	return withCOM(func() error {
		unknown, err := oleutil.CreateObject("Schedule.Service")
		if err != nil {
			return fmt.Errorf("creating the Task Scheduler service object: %w", err)
		}
		defer unknown.Release()
		service, err := unknown.QueryInterface(ole.IID_IDispatch)
		if err != nil {
			return fmt.Errorf("querying the Task Scheduler dispatch interface: %w", err)
		}
		defer service.Release()
		connected, err := oleutil.CallMethod(service, "Connect")
		if err != nil {
			return fmt.Errorf("connecting to the Task Scheduler: %w", err)
		}
		connected.Clear()
		root, err := callObject(service, "GetFolder", `\`)
		if err != nil {
			return err
		}
		defer root.Release()
		return f(service, root)
	})
}

func createTask(name string, command commandLine, trigger taskTrigger) error {
	triggerType, ok := comTriggerTypes[trigger]
	if !ok {
		return fmt.Errorf("unsupported trigger %d", trigger)
	}
	now := time.Now().UTC()
	return withTaskService(func(service, root *ole.IDispatch) error {
		def, err := callObject(service, "NewTask", 0)
		if err != nil {
			return err
		}
		defer def.Release()

		triggers, err := getObject(def, "Triggers")
		if err != nil {
			return err
		}
		defer triggers.Release()
		trig, err := callObject(triggers, "Create", triggerType)
		if err != nil {
			return err
		}
		defer trig.Release()
		for _, p := range triggerProperties(trigger, now) {
			if err := putProperty(trig, p.name, p.value); err != nil {
				return err
			}
		}

		actions, err := getObject(def, "Actions")
		if err != nil {
			return err
		}
		defer actions.Release()
		action, err := callObject(actions, "Create", taskActionExec)
		if err != nil {
			return err
		}
		defer action.Release()
		if err := putProperty(action, "Path", command.Path); err != nil {
			return err
		}
		if command.Args != "" {
			if err := putProperty(action, "Arguments", command.Args); err != nil {
				return err
			}
		}

		registered, err := callObject(root, "RegisterTaskDefinition",
			name, def, taskCreateOrUpdate, nil, nil, taskLogonInteractiveToken, nil)
		if err != nil {
			return err
		}
		registered.Release()
		return nil
	})
}

type comProperty struct {
	name  string
	value any
}

func triggerProperties(trigger taskTrigger, now time.Time) []comProperty {
	start := comProperty{"StartBoundary", now.Format(taskTimeLayout)}
	switch trigger {
	case triggerOnce:
		return []comProperty{start}
	case triggerDaily:
		return []comProperty{start, {"DaysInterval", int16(1)}}
	case triggerWeekly:
		return []comProperty{start, {"WeeksInterval", int16(1)}, {"DaysOfWeek", int16(1) << int(now.Weekday())}}
	case triggerMonthly:
		return []comProperty{start, {"DaysOfMonth", int32(1) << (now.Day() - 1)}, {"MonthsOfYear", int16(allMonths)}}
	}
	return nil
}

func deleteTask(name string) error {
	return withTaskService(func(_, root *ole.IDispatch) error {
		v, err := oleutil.CallMethod(root, "DeleteTask", name, 0)
		if err != nil {
			return err
		}
		return v.Clear()
	})
}

func listTasks() ([]scheduledTask, error) {
	var out []scheduledTask
	err := withTaskService(func(_, root *ole.IDispatch) error {
		return collectTasks(root, &out)
	})
	return out, err
}

func collectTasks(folder *ole.IDispatch, out *[]scheduledTask) error {
	tasks, err := callObject(folder, "GetTasks", taskEnumHidden)
	if err != nil {
		return err
	}
	defer tasks.Release()
	if err := forEachObject(tasks, func(task *ole.IDispatch) error {
		name, err := getValue(task, "Name")
		if err != nil {
			return err
		}
		path, err := getValue(task, "Path")
		if err != nil {
			return err
		}
		state, err := getValue(task, "State")
		if err != nil {
			return err
		}
		*out = append(*out, scheduledTask{Name: fmt.Sprint(name), Path: fmt.Sprint(path), State: taskStateName(state)})
		return nil
	}); err != nil {
		return err
	}
	folders, err := callObject(folder, "GetFolders", 0)
	if err != nil {
		return err
	}
	defer folders.Release()
	return forEachObject(folders, func(sub *ole.IDispatch) error { return collectTasks(sub, out) })
}

func forEachObject(collection *ole.IDispatch, f func(*ole.IDispatch) error) error {
	var inner error
	err := oleutil.ForEach(collection, func(v *ole.VARIANT) error {
		disp := v.ToIDispatch()
		if disp == nil {
			return errors.New("collection item is not an object")
		}
		inner = f(disp)
		return inner
	})
	if inner != nil {
		return inner
	}
	return err
}

func taskStateName(v any) string {
	var n int64
	switch x := v.(type) {
	case int32:
		n = int64(x)
	case int64:
		n = x
	case uint32:
		n = int64(x)
	default:
		return fmt.Sprint(v)
	}
	if s, ok := taskStates[n]; ok {
		return s
	}
	return "State " + strconv.FormatInt(n, 10)
}
