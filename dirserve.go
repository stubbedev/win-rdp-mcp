package main

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// The "dir" transport is how the in-session agent is reached when only RDP is
// open. The controller exposes a local directory to the session as a redirected
// drive (\\tsclient\mcp); this loop watches <dir>/req for request files and
// writes <dir>/resp/<id>.json back. It is deliberately NOT MCP framing — the
// controller has already spoken MCP to the AI and only needs to invoke a tool
// and read its result, so this is a thin request/response over files that
// reuses the exact same handlers, schema validation and concurrency limits as
// every other transport.

// dirRequest is one tool invocation written by the controller.
type dirRequest struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// dirResponse is the agent's reply. contentBlock carries text or an image (its
// Data []byte marshals as base64), so screenshots ride the same channel.
type dirResponse struct {
	ID      string         `json:"id"`
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError"`
}

const dirPollInterval = 150 * time.Millisecond

// dirLog appends a timestamped line to <dir>/agent.log, best-effort. It is the
// only window the controller has into the agent's startup, since the agent runs
// in an RDP session the controller cannot read the console of.
func dirLog(dir, format string, args ...any) {
	f, err := os.OpenFile(filepath.Join(dir, "agent.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
}

// serveDir runs the file-RPC loop until ctx is cancelled. enabled gates which
// tools answer, exactly as the network transports do.
func serveDir(ctx context.Context, dir string, enabled map[string]bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	reqDir := filepath.Join(dir, "req")
	respDir := filepath.Join(dir, "resp")
	for _, d := range []string{reqDir, respDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			dirLog(dir, "creating %s: %v", d, err)
			return fmt.Errorf("creating %s: %w", d, err)
		}
	}

	// Startup breadcrumbs go to a log on the exchange drive too, so the
	// controller (on the far side of RDP, with no view of this console) can see
	// why a bootstrap failed.
	dirLog(dir, "agent %s starting: dir transport, %d tools", Version, len(enabled))

	resolved, err := resolveSchemas()
	if err != nil {
		dirLog(dir, "schema error: %v", err)
		return err
	}

	// A readiness marker lets the controller poll for "agent is up" without
	// firing a real tool call first.
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte(Version), 0o644); err != nil {
		dirLog(dir, "could not write ready marker: %v", err)
		return err
	}
	dirLog(dir, "ready")
	logf("dir transport serving %s (%d tools)", dir, len(enabled))

	ticker := time.NewTicker(dirPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			entries, err := os.ReadDir(reqDir)
			if err != nil {
				continue
			}
			// Deterministic order so a burst of requests is handled oldest-id
			// first rather than in readdir order.
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
					names = append(names, e.Name())
				}
			}
			sort.Strings(names)
			for _, name := range names {
				handleDirRequest(ctx, reqDir, respDir, name, enabled, resolved)
			}
		}
	}
}

func handleDirRequest(ctx context.Context, reqDir, respDir, name string, enabled map[string]bool, resolved map[string]*jsonschema.Resolved) {
	reqPath := filepath.Join(reqDir, name)
	raw, err := os.ReadFile(reqPath)
	if err != nil {
		return
	}
	// Remove the request first: a malformed or duplicate file must not be
	// reprocessed on every tick.
	os.Remove(reqPath)

	var req dirRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}

	resp := runDirTool(ctx, req, enabled, resolved)
	writeDirResponse(respDir, resp)
}

// runDirTool applies the same gate → validate → run pipeline the MCP dispatch
// uses, so the dir transport cannot reach a tool the tier settings disabled.
func runDirTool(ctx context.Context, req dirRequest, enabled map[string]bool, resolved map[string]*jsonschema.Resolved) dirResponse {
	fail := func(msg string) dirResponse {
		return dirResponse{ID: req.ID, IsError: true, Content: []contentBlock{{Type: "text", Text: msg}}}
	}

	if !enabled[req.Name] {
		return fail(fmt.Sprintf("%s is not enabled on this agent", req.Name))
	}
	handler, ok := handlers[req.Name]
	if !ok {
		return fail("unknown tool: " + req.Name)
	}
	if v := resolved[req.Name]; v != nil {
		if err := v.Validate(req.Arguments); err != nil {
			return fail(fmt.Sprintf("Invalid arguments: %v", err))
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	result, err := tasks.run(callCtx, req.Name, func(ctx context.Context) (toolResult, error) {
		return handler(ctx, arguments(req.Arguments))
	})
	if err != nil {
		return fail(err.Error())
	}
	return dirResponse{ID: req.ID, Content: result.Content, IsError: result.IsError}
}

// writeDirResponse writes atomically (temp + rename) so the controller never
// reads a half-written reply.
func writeDirResponse(respDir string, resp dirResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	final := filepath.Join(respDir, resp.ID+".json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	os.Rename(tmp, final)
}

// resolveSchemas compiles every tool's input schema once, for validation.
func resolveSchemas() (map[string]*jsonschema.Resolved, error) {
	specs, err := loadToolSpecs()
	if err != nil {
		return nil, err
	}
	out := make(map[string]*jsonschema.Resolved, len(specs))
	for _, spec := range specs {
		var schema jsonschema.Schema
		if err := json.Unmarshal(spec.InputSchema, &schema); err != nil {
			return nil, fmt.Errorf("tool %s: %w", spec.Name, err)
		}
		r, err := schema.Resolve(nil)
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", spec.Name, err)
		}
		out[spec.Name] = r
	}
	return out, nil
}
