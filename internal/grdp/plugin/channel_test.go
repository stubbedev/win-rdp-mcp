package plugin

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/stubbedev/win-rdp-mcp/internal/grdp/core"
)

type recordingTransport struct {
	name     string
	messages [][]byte
}

func (r *recordingTransport) GetType() (string, uint32) { return r.name, CHANNEL_OPTION_INITIALIZED }
func (r *recordingTransport) Sender(core.ChannelSender) {}
func (r *recordingTransport) Process(s []byte)          { r.messages = append(r.messages, bytes.Clone(s)) }

func newTestChannels(names ...string) (*Channels, map[string]*recordingTransport) {
	c := &Channels{channels: map[string]ChannelClient{}}
	out := map[string]*recordingTransport{}
	for _, n := range names {
		t := &recordingTransport{name: n}
		c.Register(t)
		out[n] = t
	}
	return c, out
}

func chunk(total int, flags uint32, payload string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(total))
	b = binary.LittleEndian.AppendUint32(b, flags)
	return append(b, payload...)
}

func TestInterleavedChunksReassemblePerChannel(t *testing.T) {
	c, got := newTestChannels(RDPDR_SVC_CHANNEL_NAME, DRDYNVC_SVC_CHANNEL_NAME)

	c.process(DRDYNVC_SVC_CHANNEL_NAME, chunk(6, CHANNEL_FLAG_FIRST, "abc"))
	c.process(RDPDR_SVC_CHANNEL_NAME, chunk(4, CHANNEL_FLAG_FIRST, "12"))
	c.process(DRDYNVC_SVC_CHANNEL_NAME, chunk(6, CHANNEL_FLAG_LAST, "def"))
	c.process(RDPDR_SVC_CHANNEL_NAME, chunk(4, CHANNEL_FLAG_LAST, "34"))

	if m := got[DRDYNVC_SVC_CHANNEL_NAME].messages; len(m) != 1 || string(m[0]) != "abcdef" {
		t.Fatalf("drdynvc received %q, want [abcdef]", m)
	}
	if m := got[RDPDR_SVC_CHANNEL_NAME].messages; len(m) != 1 || string(m[0]) != "1234" {
		t.Fatalf("rdpdr received %q, want [1234]", m)
	}
}

func TestMalformedChunkSequencesAreDropped(t *testing.T) {
	c, got := newTestChannels(RDPDR_SVC_CHANNEL_NAME)

	c.process(RDPDR_SVC_CHANNEL_NAME, chunk(4, CHANNEL_FLAG_LAST, "34"))
	c.process(RDPDR_SVC_CHANNEL_NAME, chunk(9, CHANNEL_FLAG_FIRST, "12"))
	c.process(RDPDR_SVC_CHANNEL_NAME, chunk(9, CHANNEL_FLAG_LAST, "34"))
	c.process(RDPDR_SVC_CHANNEL_NAME, chunk(2, CHANNEL_FLAG_FIRST|CHANNEL_FLAG_LAST, "ok"))

	if m := got[RDPDR_SVC_CHANNEL_NAME].messages; len(m) != 1 || string(m[0]) != "ok" {
		t.Fatalf("rdpdr received %q, want only [ok]", m)
	}
}

type failingSender struct{ calls int }

func (f *failingSender) SendToChannel(string, []byte) (int, error) {
	f.calls++
	if f.calls == 2 {
		return 0, errors.New("link down")
	}
	return 0, nil
}

func TestSendToChannelReportsTransportErrors(t *testing.T) {
	c, _ := newTestChannels(RDPDR_SVC_CHANNEL_NAME)
	sender := &failingSender{}
	c.SetChannelSender(sender)

	n, err := c.SendToChannel(RDPDR_SVC_CHANNEL_NAME, make([]byte, CHANNEL_CHUNK_LENGTH*3))
	if err == nil {
		t.Fatal("a failed chunk write was not reported")
	}
	if n != CHANNEL_CHUNK_LENGTH || sender.calls != 2 {
		t.Fatalf("reported %d bytes after %d writes, want %d after 2", n, sender.calls, CHANNEL_CHUNK_LENGTH)
	}
}
