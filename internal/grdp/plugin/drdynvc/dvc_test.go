package drdynvc

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
)

type pduSink struct {
	mu   sync.Mutex
	pdus [][]byte
}

func (s *pduSink) SendToChannel(_ string, b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pdus = append(s.pdus, bytes.Clone(b))
	return len(b), nil
}

type recorder struct {
	mu      sync.Mutex
	send    func([]byte)
	got     [][]byte
	created int
	closed  int
}

func (r *recorder) Process(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, bytes.Clone(b))
}
func (r *recorder) SetSendFunc(f func([]byte)) { r.send = f }
func (r *recorder) OnChannelCreated() {
	r.created++
	r.send([]byte("hello"))
}
func (r *recorder) OnChannelClosed() { r.closed++ }

func openChannel(t *testing.T, name string) (*DvcClient, *pduSink, *recorder) {
	t.Helper()
	c := NewDvcClient()
	sink := &pduSink{}
	c.Sender(sink)
	rec := &recorder{}
	c.RegisterHandler(name, rec)
	c.Process(append([]byte{DYNVC_CREATE_REQ << 4, 7}, append([]byte(name), 0)...))
	if rec.created != 1 {
		t.Fatalf("OnChannelCreated called %d times", rec.created)
	}
	return c, sink, rec
}

func TestCreateRespondsThenLetsTheHandlerSend(t *testing.T) {
	_, sink, _ := openChannel(t, "Test")
	if len(sink.pdus) != 2 {
		t.Fatalf("got %d PDUs, want create response then data", len(sink.pdus))
	}
	if sink.pdus[0][0]>>4 != DYNVC_CREATE_REQ || binary.LittleEndian.Uint32(sink.pdus[0][2:]) != 0 {
		t.Fatalf("create response = %x", sink.pdus[0])
	}
	if sink.pdus[1][0]>>4 != DYNVC_DATA || string(sink.pdus[1][2:]) != "hello" {
		t.Fatalf("data PDU = %x", sink.pdus[1])
	}
}

func TestLargeSendsAreFragmentedAndReassemble(t *testing.T) {
	c, sink, rec := openChannel(t, "Test")
	sink.pdus = nil
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1000)
	if err := c.SendDvcData(7, payload); err != nil {
		t.Fatal(err)
	}
	if len(sink.pdus) < 2 || sink.pdus[0][0]>>4 != DYNVC_DATA_FIRST {
		t.Fatalf("%d PDUs, first %x; want a DATA_FIRST and continuation", len(sink.pdus), sink.pdus[0][:2])
	}
	for i, p := range sink.pdus {
		if len(p) > dvcMaxPDU {
			t.Fatalf("PDU %d is %d bytes, over the %d byte limit", i, len(p), dvcMaxPDU)
		}
	}
	for _, p := range sink.pdus {
		c.Process(p)
	}
	if len(rec.got) != 1 || !bytes.Equal(rec.got[0], payload) {
		t.Fatalf("reassembled %d messages, first %d bytes; want the %d byte payload once", len(rec.got), len(rec.got[0]), len(payload))
	}
}

func TestCloseNotifiesAndStopsSends(t *testing.T) {
	c, _, rec := openChannel(t, "Test")
	c.Process([]byte{DYNVC_CLOSE << 4, 7})
	if rec.closed != 1 {
		t.Fatalf("OnChannelClosed called %d times", rec.closed)
	}
	if err := c.SendDvcData(7, []byte("x")); err == nil {
		t.Fatal("send on a closed channel succeeded")
	}
}
