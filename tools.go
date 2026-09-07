package main

import (
	"context"
	_ "embed"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"slices"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed tools.json
var toolsJSON []byte

//go:embed package.json
var packageJSON []byte

// Version is read from the embedded package.json, the one place the release
// flow bumps — so `go build`, `go install`, Nix and the release binaries all
// report the same number with no -ldflags wiring.
var Version = versionFromPackage()

func versionFromPackage() string {
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(packageJSON, &pkg); err == nil && pkg.Version != "" {
		return pkg.Version
	}
	return "dev"
}

// toolFunc is one tool's implementation. Returning an error marks the call
// failed; the message reaches the model so it can correct itself.
type toolFunc func(context.Context, arguments) (toolResult, error)

// handlers maps every name in tools.json onto its implementation. A name here
// with no schema (or the reverse) is caught by TestToolsMatchSchemas.
var handlers = map[string]toolFunc{
	"Snapshot":          snapshotTool,
	"AnnotatedSnapshot": annotatedSnapshotTool,
	"Click":             clickTool,
	"Type":              typeTool,
	"Scroll":            scrollTool,
	"Move":              moveTool,
	"Shortcut":          shortcutTool,
	"Wait":              waitTool,
	"FocusWindow":       focusWindowTool,
	"MinimizeAll":       minimizeAllTool,
	"App":               appTool,
	"Shell":             shellTool,
	"GetClipboard":      getClipboardTool,
	"SetClipboard":      setClipboardTool,
	"ListProcesses":     listProcessesTool,
	"KillProcess":       killProcessTool,
	"GetSystemInfo":     systemInfoTool,
	"ReconnectSession":  reconnectSessionTool,
	"Notification":      notificationTool,
	"PlaySound":         playSoundTool,
	"LockScreen":        lockScreenTool,
	"Scrape":            scrapeTool,
	"FileRead":          fileReadTool,
	"FileWrite":         fileWriteTool,
	"FileList":          fileListTool,
	"FileSearch":        fileSearchTool,
	"FileDownload":      fileDownloadTool,
	"FileUpload":        fileUploadTool,
	"RegRead":           regReadTool,
	"RegWrite":          regWriteTool,
	"ServiceList":       serviceListTool,
	"ServiceStart":      serviceStartTool,
	"ServiceStop":       serviceStopTool,
	"TaskList":          taskListTool,
	"TaskCreate":        taskCreateTool,
	"TaskDelete":        taskDeleteTool,
	"Ping":              pingTool,
	"PortCheck":         portCheckTool,
	"NetConnections":    netConnectionsTool,
	"EventLog":          eventLogTool,
	"OCR":               ocrTool,
	"ScreenRecord":      screenRecordTool,
	"CancelTask":        cancelTaskTool,
	"GetTaskStatus":     getTaskStatusTool,
	"GetRunningTasks":   getRunningTasksTool,
}

// toolSpec is one entry of tools.json.
type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema jsontext.Value `json:"inputSchema"`
}

func loadToolSpecs() ([]toolSpec, error) {
	var specs []toolSpec
	if err := json.Unmarshal(toolsJSON, &specs); err != nil {
		return nil, fmt.Errorf("parsing embedded tools.json: %w", err)
	}
	return specs, nil
}

// toolCallTimeout caps one call's total wall clock. ScreenRecord and a slow
// PowerShell command are the long tails; anything past this is wedged.
const toolCallTimeout = 3 * time.Minute

// registerTools adds every enabled tool to the server, compiling its schema for
// argument validation as it goes.
func registerTools(srv *mcp.Server, enabled map[string]bool) error {
	specs, err := loadToolSpecs()
	if err != nil {
		return err
	}

	for _, spec := range specs {
		if !enabled[spec.Name] {
			continue
		}
		handler, ok := handlers[spec.Name]
		if !ok {
			return fmt.Errorf("tool %s has a schema but no implementation", spec.Name)
		}

		var schema jsonschema.Schema
		if err := json.Unmarshal(spec.InputSchema, &schema); err != nil {
			return fmt.Errorf("tool %s: parsing input schema: %w", spec.Name, err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			return fmt.Errorf("tool %s: resolving input schema: %w", spec.Name, err)
		}

		tool := &mcp.Tool{
			Name:        spec.Name,
			Description: spec.Description,
			InputSchema: &schema,
			Annotations: annotationsFor(spec.Name),
		}
		srv.AddTool(tool, dispatch(spec.Name, handler, resolved))
	}
	return nil
}

// annotationsFor turns the tier a tool sits in into the protocol's hints, so a
// client can warn before a destructive call without knowing the tier scheme.
func annotationsFor(name string) *mcp.ToolAnnotations {
	destructive := slices.Contains(tier3, name)
	openWorld := slices.Contains([]string{"Scrape", "Ping", "PortCheck", "App", "Shell", "PlaySound"}, name)
	return &mcp.ToolAnnotations{
		Title:           name,
		ReadOnlyHint:    slices.Contains(tier1, name),
		DestructiveHint: &destructive,
		OpenWorldHint:   &openWorld,
	}
}

// dispatch validates arguments, then runs the tool under the task manager's
// per-category concurrency limit.
func dispatch(name string, handler toolFunc, schema *jsonschema.Resolved) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, toolCallTimeout)
		defer cancel()

		args, err := decodeArguments(req.Params.Arguments)
		if err != nil {
			return errorResult(fmt.Sprintf("Invalid arguments: %v", err)), nil
		}
		if err := schema.Validate(map[string]any(args)); err != nil {
			return errorResult(fmt.Sprintf("Invalid arguments: %v", err)), nil
		}

		result, err := tasks.run(ctx, name, func(ctx context.Context) (toolResult, error) {
			return handler(ctx, args)
		})
		if err != nil {
			// A tool failure is a result the model reads and reacts to, not a
			// protocol error that aborts the conversation. The task manager has
			// already framed the message with the task id and the tool name.
			return errorResult(err.Error()), nil
		}
		return toCallResult(result), nil
	}
}

// decodeArguments normalizes whatever shape the transport handed back into a
// plain map: already-decoded objects pass through, anything else round-trips
// through JSON.
func decodeArguments(raw any) (arguments, error) {
	switch v := raw.(type) {
	case nil:
		return arguments{}, nil
	case map[string]any:
		return arguments(v), nil
	case arguments:
		return v, nil
	case jsontext.Value:
		return unmarshalArguments(v)
	case []byte:
		// Raw JSON, not a byte payload: unmarshal it rather than round-tripping,
		// which would base64-encode it into a string.
		return unmarshalArguments(v)
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return unmarshalArguments(encoded)
}

func unmarshalArguments(encoded []byte) (arguments, error) {
	if len(encoded) == 0 {
		return arguments{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func toCallResult(r toolResult) *mcp.CallToolResult {
	out := &mcp.CallToolResult{IsError: r.IsError}
	for _, c := range r.Content {
		if c.Type == "image" {
			out.Content = append(out.Content, &mcp.ImageContent{Data: c.Data, MIMEType: c.MIME})
			continue
		}
		out.Content = append(out.Content, &mcp.TextContent{Text: c.Text})
	}
	return out
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}
