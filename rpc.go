package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	agentChannelName = "WinRdpMcp"
	maxRPCFrame      = 64 << 20
	rpcFrameHeader   = 4
)

type rpcKind string

const (
	rpcHello  rpcKind = "hello"
	rpcCall   rpcKind = "call"
	rpcResult rpcKind = "result"
	rpcBye    rpcKind = "bye"

	rpcRestoreClipboard rpcKind = "restoreClipboard"
)

type rpcMessage struct {
	Kind      rpcKind        `json:"kind"`
	ID        string         `json:"id,omitzero"`
	Token     string         `json:"token,omitzero"`
	Version   string         `json:"version,omitzero"`
	Name      string         `json:"name,omitzero"`
	Arguments map[string]any `json:"arguments,omitzero"`
	Content   []contentBlock `json:"content,omitzero"`
	IsError   bool           `json:"isError,omitzero"`
	Text      string         `json:"text,omitzero"`
}

var errAgentRetired = errors.New("the controller retired this agent")

const agentTokenBytes = 8

func newAgentToken() (string, error) {
	var b [agentTokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating an agent token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func validAgentToken(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == agentTokenBytes
}

func encodeFrame(msg rpcMessage) ([]byte, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encoding %s message: %w", msg.Kind, err)
	}
	if len(body) > maxRPCFrame {
		return nil, fmt.Errorf("%s message is %d bytes, over the %d byte frame limit", msg.Kind, len(body), maxRPCFrame)
	}
	frame := make([]byte, rpcFrameHeader, rpcFrameHeader+len(body))
	binary.LittleEndian.PutUint32(frame, uint32(len(body)))
	return append(frame, body...), nil
}

type frameReader struct {
	buf []byte
}

func (f *frameReader) push(data []byte) ([]rpcMessage, error) {
	f.buf = append(f.buf, data...)
	var out []rpcMessage
	for len(f.buf) >= rpcFrameHeader {
		n := int(binary.LittleEndian.Uint32(f.buf))
		if n > maxRPCFrame {
			return out, fmt.Errorf("frame of %d bytes exceeds the %d byte limit", n, maxRPCFrame)
		}
		if len(f.buf) < rpcFrameHeader+n {
			break
		}
		var msg rpcMessage
		if err := json.Unmarshal(f.buf[rpcFrameHeader:rpcFrameHeader+n], &msg); err != nil {
			return out, fmt.Errorf("decoding a frame: %w", err)
		}
		out = append(out, msg)
		f.buf = f.buf[rpcFrameHeader+n:]
	}
	if len(f.buf) == 0 {
		f.buf = nil
	}
	return out, nil
}

type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (fw *frameWriter) send(msg rpcMessage) error {
	frame, err := encodeFrame(msg)
	if err != nil {
		return err
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if _, err := fw.w.Write(frame); err != nil {
		return fmt.Errorf("sending %s message: %w", msg.Kind, err)
	}
	return nil
}

func (fw *frameWriter) answer(result rpcMessage, what string) {
	err := fw.send(result)
	if err == nil {
		return
	}
	logf("answering %s (%s): %v", result.ID, what, err)
	fallback := rpcMessage{Kind: rpcResult, ID: result.ID, IsError: true,
		Content: []contentBlock{{Type: "text", Text: fmt.Sprintf("the agent could not send the %s result: %v", what, err)}}}
	if err := fw.send(fallback); err != nil {
		logf("answering %s (%s) with the failure: %v", result.ID, what, err)
	}
}

func serveRPC(ctx context.Context, conn io.ReadWriter, token string, enabled map[string]bool, resolved map[string]*jsonschema.Resolved) error {
	var calls sync.WaitGroup
	defer calls.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &frameWriter{w: conn}
	if err := out.send(rpcMessage{Kind: rpcHello, Token: token, Version: Version}); err != nil {
		return err
	}
	var in frameReader
	buf := make([]byte, 64<<10)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return fmt.Errorf("reading from the controller: %w", err)
		}
		msgs, err := in.push(buf[:n])
		for _, msg := range msgs {
			switch msg.Kind {
			case rpcBye:
				return errAgentRetired
			case rpcCall:
				calls.Go(func() { out.answer(runRPCTool(ctx, msg, enabled, resolved), msg.Name) })
			case rpcRestoreClipboard:
				calls.Go(func() {
					result := rpcMessage{Kind: rpcResult, ID: msg.ID}
					if err := setClipboard(msg.Text); err != nil {
						result.IsError = true
						result.Content = []contentBlock{{Type: "text", Text: "restoring the clipboard: " + err.Error()}}
					}
					out.answer(result, "clipboard restore")
				})
			default:
				logf("ignoring unexpected %q message from the controller", msg.Kind)
			}
		}
		if err != nil {
			return err
		}
	}
}

func runRPCTool(ctx context.Context, req rpcMessage, enabled map[string]bool, resolved map[string]*jsonschema.Resolved) rpcMessage {
	fail := func(msg string) rpcMessage {
		return rpcMessage{Kind: rpcResult, ID: req.ID, IsError: true, Content: []contentBlock{{Type: "text", Text: msg}}}
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
	return rpcMessage{Kind: rpcResult, ID: req.ID, Content: result.Content, IsError: result.IsError}
}

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
