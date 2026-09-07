package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
)

// Tools are grouped by what they contend on, and each group gets its own
// concurrency budget. Desktop work is exclusive: two agents moving the mouse at
// once produce nonsense, so the mouse is a single-holder resource.
type toolCategory string

const (
	catDesktop toolCategory = "desktop"
	catFile    toolCategory = "file"
	catQuery   toolCategory = "query"
	catShell   toolCategory = "shell"
	catNetwork toolCategory = "network"
)

var categoryLimits = map[toolCategory]int{
	catDesktop: 1,
	catFile:    5,
	catQuery:   10,
	catShell:   3,
	catNetwork: 5,
}

var toolCategories = map[string]toolCategory{
	"Snapshot": catDesktop, "AnnotatedSnapshot": catDesktop, "Click": catDesktop,
	"Type": catDesktop, "Scroll": catDesktop, "Move": catDesktop,
	"Shortcut": catDesktop, "FocusWindow": catDesktop, "MinimizeAll": catDesktop,
	"App": catDesktop, "OCR": catDesktop, "ScreenRecord": catDesktop,
	"LockScreen": catDesktop, "Wait": catDesktop,

	"FileRead": catFile, "FileWrite": catFile, "FileList": catFile,
	"FileSearch": catFile, "FileDownload": catFile, "FileUpload": catFile,

	"GetSystemInfo": catQuery, "GetClipboard": catQuery, "SetClipboard": catQuery,
	"ListProcesses": catQuery, "KillProcess": catQuery, "Notification": catQuery,
	"PlaySound": catQuery, "ReconnectSession": catQuery,
	"RegRead": catQuery, "RegWrite": catQuery,
	"ServiceList": catQuery, "ServiceStart": catQuery, "ServiceStop": catQuery,
	"TaskList": catQuery, "TaskCreate": catQuery, "TaskDelete": catQuery,
	"EventLog": catQuery,

	"Shell": catShell, "Scrape": catShell,

	"Ping": catNetwork, "PortCheck": catNetwork, "NetConnections": catNetwork,
}

func categoryFor(tool string) toolCategory {
	if c, ok := toolCategories[tool]; ok {
		return c
	}
	return catQuery
}

type taskStatus string

const (
	statusPending   taskStatus = "pending"
	statusRunning   taskStatus = "running"
	statusCompleted taskStatus = "completed"
	statusFailed    taskStatus = "failed"
	statusCancelled taskStatus = "cancelled"
)

type taskInfo struct {
	ID       string       `json:"task_id"`
	Tool     string       `json:"tool_name"`
	Category toolCategory `json:"category"`
	Status   taskStatus   `json:"status"`
	Duration *float64     `json:"duration"`
	Error    string       `json:"error,omitempty"`

	createdAt   time.Time
	startedAt   time.Time
	completedAt time.Time
	cancel      context.CancelFunc
}

// snapshot returns a copy safe to hand out; the caller holds no lock on it.
func (t *taskInfo) snapshot() taskInfo {
	out := *t
	if !t.startedAt.IsZero() {
		end := t.completedAt
		if end.IsZero() {
			end = time.Now()
		}
		d := end.Sub(t.startedAt).Round(10 * time.Millisecond).Seconds()
		out.Duration = &d
	}
	out.cancel = nil
	return out
}

// maxTaskHistory caps how many finished tasks are kept for GetTaskStatus.
const maxTaskHistory = 100

// acquireTimeout is how long a tool waits for its category slot before giving
// up. Long enough for a slow screenshot to finish, short enough that a wedged
// desktop tool does not hang the client.
const acquireTimeout = 30 * time.Second

type taskManager struct {
	mu    sync.Mutex
	tasks map[string]*taskInfo
	sems  map[toolCategory]chan struct{}
}

func newTaskManager() *taskManager {
	sems := make(map[toolCategory]chan struct{}, len(categoryLimits))
	for cat, limit := range categoryLimits {
		sems[cat] = make(chan struct{}, limit)
	}
	return &taskManager{tasks: map[string]*taskInfo{}, sems: sems}
}

func newTaskID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// run executes fn under the tool's category budget, tracking it as a task so
// GetTaskStatus and CancelTask can see it. The returned result carries a
// [task:id] prefix so the model can refer back to this call.
func (m *taskManager) run(ctx context.Context, tool string, fn func(context.Context) (toolResult, error)) (toolResult, error) {
	cat := categoryFor(tool)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	task := &taskInfo{
		ID: newTaskID(), Tool: tool, Category: cat,
		Status: statusPending, createdAt: time.Now(), cancel: cancel,
	}
	m.mu.Lock()
	m.tasks[task.ID] = task
	m.pruneLocked()
	m.mu.Unlock()

	sem := m.sems[cat]
	acquireCtx, acquireCancel := context.WithTimeout(ctx, acquireTimeout)
	defer acquireCancel()

	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-acquireCtx.Done():
		msg := fmt.Sprintf("timeout waiting for the %s lock (another %s task is running)", cat, cat)
		m.finish(task, statusFailed, msg)
		return toolResult{}, fmt.Errorf("[task:%s] %s error: %s", task.ID, tool, msg)
	}

	m.mu.Lock()
	cancelled := task.Status == statusCancelled
	if !cancelled {
		task.Status = statusRunning
		task.startedAt = time.Now()
	}
	m.mu.Unlock()
	if cancelled {
		return textResult("[task:%s] Cancelled before execution", task.ID), nil
	}

	result, err := fn(ctx)
	if err != nil {
		m.finish(task, statusFailed, err.Error())
		// The task id leads, so a failure reads like every successful result
		// and the model can quote the same id back to GetTaskStatus.
		return toolResult{}, fmt.Errorf("[task:%s] %s error: %w", task.ID, tool, err)
	}
	m.finish(task, statusCompleted, "")
	return result.withTaskID(task.ID), nil
}

func (m *taskManager) finish(task *taskInfo, status taskStatus, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// A task cancelled mid-flight keeps that verdict rather than being
	// overwritten by the failure its own cancellation caused.
	if task.Status != statusCancelled {
		task.Status = status
		task.Error = errMsg
	}
	task.completedAt = time.Now()
}

func (m *taskManager) cancelTask(id string) (taskInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return taskInfo{}, fmt.Errorf("task %s not found", id)
	}
	if task.Status != statusPending && task.Status != statusRunning {
		return taskInfo{}, fmt.Errorf("task %s is already %s", id, task.Status)
	}
	task.Status = statusCancelled
	task.completedAt = time.Now()
	if task.cancel != nil {
		task.cancel()
	}
	return task.snapshot(), nil
}

func (m *taskManager) get(id string) (taskInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[id]
	if !ok {
		return taskInfo{}, false
	}
	return task.snapshot(), true
}

// list returns tasks newest first, optionally filtered by status.
func (m *taskManager) list(status taskStatus) []taskInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []taskInfo
	for task := range maps.Values(m.tasks) {
		if status != "" && task.Status != status {
			continue
		}
		out = append(out, task.snapshot())
	}
	slices.SortFunc(out, func(a, b taskInfo) int { return b.createdAt.Compare(a.createdAt) })
	return out
}

// pruneLocked trims finished tasks past maxTaskHistory, oldest first. Caller
// holds m.mu.
func (m *taskManager) pruneLocked() {
	var done []*taskInfo
	for task := range maps.Values(m.tasks) {
		switch task.Status {
		case statusCompleted, statusFailed, statusCancelled:
			done = append(done, task)
		}
	}
	if len(done) <= maxTaskHistory {
		return
	}
	slices.SortFunc(done, func(a, b *taskInfo) int { return a.createdAt.Compare(b.createdAt) })
	for _, task := range done[:len(done)-maxTaskHistory] {
		delete(m.tasks, task.ID)
	}
}
