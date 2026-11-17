package sec

import (
	"bytes"
	"encoding/hex"
	"log/slog"
	"strings"
	"testing"

	"github.com/stubbedev/win-rdp-mcp/internal/grdp/emission"
	"github.com/stubbedev/win-rdp-mcp/internal/grdp/protocol/t125/gcc"
)

type recordingTransport struct {
	emission.Emitter
	written bytes.Buffer
}

func (r *recordingTransport) Read([]byte) (int, error)    { return 0, nil }
func (r *recordingTransport) Write(b []byte) (int, error) { return r.written.Write(b) }
func (r *recordingTransport) Close() error                { return nil }

func captureDebugLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

func TestInfoPacketCarriesThePasswordWithoutLoggingIt(t *testing.T) {
	logs := captureDebugLogs(t)
	transport := &recordingTransport{Emitter: *emission.NewEmitter()}
	c := NewClient(transport)
	c.clientData = []any{gcc.NewClientCoreData(uint32(gcc.US), uint32(gcc.KT_IBM_101_102_KEYS), 0), gcc.NewClientSecurityData(), gcc.NewClientNetworkData()}
	const password = "Tr0ub4dor&3"
	c.SetPwd(password)

	c.sendInfoPkt()

	encoded := c.info.Password[:len(c.info.Password)-2]
	if !bytes.Contains(transport.written.Bytes(), encoded) {
		t.Fatal("the info packet does not carry the password")
	}
	out := logs.String()
	if strings.Contains(out, hex.EncodeToString(encoded)) || strings.Contains(out, password) {
		t.Errorf("the password reached the log:\n%s", out)
	}
	if !strings.Contains(out, "sendFlagged") {
		t.Errorf("expected the send to be logged at debug level, got:\n%s", out)
	}
}
