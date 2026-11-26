package pdu

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
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

func confirmActiveCapabilitySets(t *testing.T, b []byte) map[CapsType][]byte {
	t.Helper()
	const shareControlHeaderLen = 6
	r := bytes.NewReader(b[shareControlHeaderLen:])
	var fixed struct {
		SharedID                   uint32
		OriginatorID               uint16
		LengthSourceDescriptor     uint16
		LengthCombinedCapabilities uint16
	}
	if err := binary.Read(r, binary.LittleEndian, &fixed); err != nil {
		t.Fatalf("reading the Confirm Active PDU header: %v", err)
	}
	if _, err := r.Seek(int64(fixed.LengthSourceDescriptor), 1); err != nil {
		t.Fatalf("skipping the source descriptor: %v", err)
	}
	var counts struct {
		NumberCapabilities uint16
		Pad2Octets         uint16
	}
	if err := binary.Read(r, binary.LittleEndian, &counts); err != nil {
		t.Fatalf("reading the capability count: %v", err)
	}
	sets := make(map[CapsType][]byte, counts.NumberCapabilities)
	for range counts.NumberCapabilities {
		var capHead struct {
			Type   uint16
			Length uint16
		}
		if err := binary.Read(r, binary.LittleEndian, &capHead); err != nil {
			t.Fatalf("reading a capability set header: %v", err)
		}
		body := make([]byte, int(capHead.Length)-4)
		if _, err := io.ReadFull(r, body); err != nil {
			t.Fatalf("reading capability set 0x%04x: %v", capHead.Type, err)
		}
		set := binary.LittleEndian.AppendUint16(nil, capHead.Type)
		set = binary.LittleEndian.AppendUint16(set, capHead.Length)
		sets[CapsType(capHead.Type)] = append(set, body...)
	}
	return sets
}

func TestConfirmActiveGeneralCapabilitySetLeavesCompressionTypesZero(t *testing.T) {
	transport := &recordingTransport{Emitter: *emission.NewEmitter()}
	c := NewClient(transport)
	c.clientCoreData = gcc.NewClientCoreData(uint32(gcc.US), uint32(gcc.KT_IBM_101_102_KEYS), 0)
	c.demandActivePDU = &DemandActivePDU{SourceDescriptor: []byte("RDP")}

	c.sendConfirmActivePDU()

	got, ok := confirmActiveCapabilitySets(t, transport.written.Bytes())[CAPSTYPE_GENERAL]
	if !ok {
		t.Fatal("the Confirm Active PDU carries no General Capability Set")
	}
	want, _ := hex.DecodeString("0100" + "1800" + "0100" + "0300" + "0002" + "0000" + "0000" + "0d04" + "0000" + "0000" + "0000" + "01" + "01")
	if !bytes.Equal(got, want) {
		t.Errorf("General Capability Set\n got %x\nwant %x", got, want)
	}
}

func TestReadPDURejectsBulkCompressedShareDataPDU(t *testing.T) {
	header := &ShareDataHeader{PDUType2: PDUTYPE2_UPDATE, CompressedType: 0x21}
	var share bytes.Buffer
	if err := binary.Write(&share, binary.LittleEndian, header); err != nil {
		t.Fatal(err)
	}
	share.Write([]byte{0xde, 0xad, 0xbe, 0xef})
	var pdu bytes.Buffer
	if err := binary.Write(&pdu, binary.LittleEndian, ShareControlHeader{
		TotalLength: uint16(6 + share.Len()),
		PDUType:     PDUTYPE_DATAPDU,
	}); err != nil {
		t.Fatal(err)
	}
	pdu.Write(share.Bytes())

	_, err := readPDU(bytes.NewReader(pdu.Bytes()))
	if err == nil || !strings.Contains(err.Error(), "compressedType 0x21") {
		t.Fatalf("want an error naming the unnegotiated compressedType, got %v", err)
	}
}
