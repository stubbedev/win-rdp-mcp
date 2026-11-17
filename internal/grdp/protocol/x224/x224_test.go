package x224

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/stubbedev/win-rdp-mcp/internal/grdp/emission"
)

type fakeTransport struct {
	emission.Emitter
	closed bool
}

func (f *fakeTransport) Read([]byte) (int, error)    { return 0, nil }
func (f *fakeTransport) Write(b []byte) (int, error) { return len(b), nil }
func (f *fakeTransport) Close() error                { f.closed = true; return nil }

func confirmWithoutNegotiation() []byte {
	return []byte{6, byte(TPDU_CONNECTION_CONFIRM), 0, 0, 0, 0, 0}
}

func confirmSelecting(protocol uint32) []byte {
	b := []byte{14, byte(TPDU_CONNECTION_CONFIRM), 0, 0, 0, 0, 0, TYPE_RDP_NEG_RSP, 0, 8, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[11:], protocol)
	return b
}

func connectionConfirmError(t *testing.T, confirm []byte) (error, bool) {
	t.Helper()
	transport := &fakeTransport{Emitter: *emission.NewEmitter()}
	x := New(transport)
	var got error
	x.On("error", func(err error) { got = err })
	x.On("connect", func(uint32) { t.Fatal("connected despite an unacceptable security protocol") })
	x.recvConnectionConfirm(confirm)
	return got, transport.closed
}

func TestConnectionConfirmRefusesStandardRDPSecurity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		confirm []byte
	}{
		{"no negotiation response", confirmWithoutNegotiation()},
		{"negotiated PROTOCOL_RDP", confirmSelecting(PROTOCOL_RDP)},
	} {
		err, closed := connectionConfirmError(t, tc.confirm)
		if !errors.Is(err, ErrStandardRDPSecurity) {
			t.Errorf("%s: err = %v, want ErrStandardRDPSecurity", tc.name, err)
		}
		if !closed {
			t.Errorf("%s: the transport was left open", tc.name)
		}
	}
}

func TestConnectionConfirmRefusesAProtocolThatWasNotOffered(t *testing.T) {
	err, closed := connectionConfirmError(t, confirmSelecting(PROTOCOL_HYBRID_EX))
	if err == nil || !strings.Contains(err.Error(), "not offered") {
		t.Errorf("err = %v, want a not-offered error", err)
	}
	if !closed {
		t.Error("the transport was left open")
	}
}
