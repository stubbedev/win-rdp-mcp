package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type pipeEnd struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
}

func newPipeEnd() *pipeEnd {
	p := &pipeEnd{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *pipeEnd) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	p.buf.Write(b)
	p.cond.Broadcast()
	return len(b), nil
}

func (p *pipeEnd) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.buf.Len() == 0 && !p.closed {
		p.cond.Wait()
	}
	if p.buf.Len() == 0 {
		return 0, io.EOF
	}
	return p.buf.Read(b)
}

func (p *pipeEnd) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.cond.Broadcast()
}

type duplex struct {
	r *pipeEnd
	w *pipeEnd
}

func (d duplex) Read(b []byte) (int, error)  { return d.r.Read(b) }
func (d duplex) Write(b []byte) (int, error) { return d.w.Write(b) }

type agentHarness struct {
	link       *agentLink
	conn       *agentConn
	toAgent    *pipeEnd
	toCtl      *pipeEnd
	agentDone  chan error
	readerDone chan struct{}
}

func startAgent(t *testing.T, linkBuild, agentBuild string, enabled map[string]bool) *agentHarness {
	t.Helper()
	resolved, err := resolveSchemas()
	if err != nil {
		t.Fatal(err)
	}
	h := &agentHarness{
		link: newAgentLink(linkBuild, toolNames(enabled)), toAgent: newPipeEnd(), toCtl: newPipeEnd(),
		agentDone: make(chan error, 1), readerDone: make(chan struct{}),
	}
	h.conn = h.link.NewChannelHandler().(*agentConn)
	h.conn.SetSendFunc(func(f []byte) { h.toAgent.Write(f) })
	go func() {
		h.agentDone <- serveRPC(context.Background(), duplex{r: h.toAgent, w: h.toCtl}, agentBuild, resolved)
		h.toCtl.Close()
	}()
	go func() {
		defer close(h.readerDone)
		buf := make([]byte, 1500)
		for {
			n, err := h.toCtl.Read(buf)
			if err != nil {
				h.conn.OnChannelClosed()
				return
			}
			h.conn.Process(bytes.Clone(buf[:n]))
		}
	}()
	t.Cleanup(func() {
		h.toAgent.Close()
		<-h.readerDone
	})
	return h
}

func TestFrameReaderHandlesSplitAndBatchedFrames(t *testing.T) {
	a, _ := encodeFrame(rpcMessage{Kind: rpcCall, ID: "1", Name: "Wait"})
	b, _ := encodeFrame(rpcMessage{Kind: rpcBye})
	stream := append(a, b...)
	var fr frameReader
	var got []rpcMessage
	for _, chunk := range [][]byte{stream[:3], stream[3:10], stream[10:]} {
		msgs, err := fr.push(chunk)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, msgs...)
	}
	if len(got) != 2 || got[0].Name != "Wait" || got[1].Kind != rpcBye {
		t.Fatalf("decoded %+v", got)
	}
}

func TestFrameReaderRejectsOversizedFrames(t *testing.T) {
	var fr frameReader
	if _, err := fr.push([]byte{0xFF, 0xFF, 0xFF, 0x7F}); err == nil {
		t.Fatal("an oversized frame header was accepted")
	}
}

func TestAgentAttachesAndAnswersConcurrentCalls(t *testing.T) {
	h := startAgent(t, "aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaa", map[string]bool{"Wait": true})
	conn, err := h.link.wait(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			res, err := conn.call(context.Background(), "Wait", map[string]any{"seconds": 0.05})
			if err != nil {
				errs <- err
				return
			}
			if res.IsError || !strings.Contains(firstText(res.Content), "Waited") {
				errs <- errors.New("unexpected result: " + firstText(res.Content))
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	res, err := conn.call(context.Background(), "Shell", map[string]any{"command": "x"})
	if err != nil || !res.IsError || !strings.Contains(firstText(res.Content), "not enabled") {
		t.Fatalf("a disabled tool answered %+v, %v", res, err)
	}
}

func TestStaleAgentIsRetired(t *testing.T) {
	h := startAgent(t, "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", map[string]bool{})
	select {
	case err := <-h.agentDone:
		if !errors.Is(err, errAgentRetired) {
			t.Fatalf("stale agent ended with %v, want retirement", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stale agent was not retired")
	}
	if h.link.connection() != nil {
		t.Fatal("a stale agent was attached")
	}
}

func TestChannelCloseFailsPendingCalls(t *testing.T) {
	h := startAgent(t, "aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaa", map[string]bool{"Wait": true})
	conn, err := h.link.wait(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.call(context.Background(), "Wait", map[string]any{"seconds": 30})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	h.conn.OnChannelClosed()
	select {
	case err := <-done:
		if !errors.Is(err, errAgentChannelClosed) {
			t.Fatalf("pending call ended with %v, want a channel-closed error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a pending call outlived its channel")
	}
	if h.link.connection() != nil {
		t.Fatal("the closed connection is still attached")
	}
}

func TestRestoreClipboardReportsAgentErrors(t *testing.T) {
	h := startAgent(t, "aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaa", map[string]bool{})
	conn, err := h.link.wait(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = conn.restoreClipboard(context.Background(), "previous text")
	if runtime.GOOS == "windows" {
		if err != nil {
			t.Fatalf("restoring the clipboard on Windows: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "restoring the clipboard") {
		t.Fatalf("restore off Windows returned %v, want the agent's error", err)
	}
}

func TestUnencodableResultIsAnsweredWithAnError(t *testing.T) {
	var buf bytes.Buffer
	out := &frameWriter{w: &buf}
	out.answer(rpcMessage{Kind: rpcResult, ID: "42", Content: []contentBlock{{Type: "text", Text: "45\xf8F"}}}, "OCR")
	var fr frameReader
	msgs, err := fr.push(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != "42" || !msgs[0].IsError || !strings.Contains(firstText(msgs[0].Content), "could not send the OCR result") {
		t.Fatalf("got %+v, want one error result for call 42", msgs)
	}
}

func TestDropAllFailsPendingCalls(t *testing.T) {
	h := startAgent(t, "aaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaa", map[string]bool{"Wait": true})
	conn, err := h.link.wait(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.call(context.Background(), "Wait", map[string]any{"seconds": 30})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	h.link.dropAll()
	select {
	case err := <-done:
		if !errors.Is(err, errAgentChannelClosed) {
			t.Fatalf("pending call ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dropAll did not fail the pending call")
	}
}

func TestAgentServesNothingUntilConfigured(t *testing.T) {
	resolved, err := resolveSchemas()
	if err != nil {
		t.Fatal(err)
	}
	toAgent, toCtl := newPipeEnd(), newPipeEnd()
	go serveRPC(context.Background(), duplex{r: toAgent, w: toCtl}, "aaaaaaaaaaaaaaaa", resolved)
	t.Cleanup(toAgent.Close)

	call, _ := encodeFrame(rpcMessage{Kind: rpcCall, ID: "1", Name: "Wait", Arguments: map[string]any{"seconds": 0.01}})
	toAgent.Write(call)
	var fr frameReader
	buf := make([]byte, 4096)
	var got []rpcMessage
	for len(got) < 2 {
		n, err := toCtl.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		msgs, err := fr.push(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, msgs...)
	}
	if got[0].Kind != rpcHello || got[1].ID != "1" || !got[1].IsError || !strings.Contains(firstText(got[1].Content), "not enabled") {
		t.Fatalf("an unconfigured agent answered %+v", got)
	}
}
