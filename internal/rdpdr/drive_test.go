package rdpdr

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type capture struct{ packets [][]byte }

func (c *capture) SendToChannel(channel string, s []byte) (int, error) {
	if channel != "rdpdr" {
		panic("unexpected channel " + channel)
	}
	c.packets = append(c.packets, bytes.Clone(s))
	return len(s), nil
}

func (c *capture) take(t *testing.T) []byte {
	t.Helper()
	if len(c.packets) == 0 {
		t.Fatal("expected a packet, got none")
	}
	p := c.packets[0]
	c.packets = c.packets[1:]
	return p
}

func newTestDrive(t *testing.T) (*Drive, *capture, string) {
	t.Helper()
	dir := t.TempDir()
	d, err := NewDrive("mcp", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c := &capture{}
	d.Sender(c)
	return d, c, dir
}

func packet(component, id uint16, body ...[]byte) []byte {
	w := header(component, id)
	for _, b := range body {
		w.bytes(b)
	}
	return w.buf
}

const accessDelete = 0x00010000

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func TestHandshakeAnnouncesDriveAfterLogon(t *testing.T) {
	d, c, _ := newTestDrive(t)

	d.Process(packet(componentCore, packetServerAnnounce, []byte{1, 0, 0x0D, 0}, le32(7)))
	confirm := &reader{buf: c.take(t)}
	if confirm.u16() != componentCore || confirm.u16() != packetClientIDConfirm {
		t.Fatal("first reply is not a client ID confirm")
	}
	confirm.u16()
	confirm.u16()
	if id := confirm.u32(); id != 7 {
		t.Fatalf("client id echoed as %d, want 7", id)
	}
	name := &reader{buf: c.take(t)}
	name.u16()
	if name.u16() != packetClientName {
		t.Fatal("second reply is not a client name")
	}

	d.Process(packet(componentCore, packetServerCapability, []byte{0, 0, 0, 0}))
	caps := &reader{buf: c.take(t)}
	caps.u16()
	if caps.u16() != packetClientCapability {
		t.Fatal("no client capability response")
	}

	d.Process(packet(componentCore, packetClientIDConfirm, []byte{1, 0, 0x0D, 0}, le32(7)))
	if len(c.packets) != 0 {
		t.Fatal("drive announced before user logon on a modern server")
	}

	d.Process(packet(componentCore, packetUserLoggedOn))
	ann := &reader{buf: c.take(t)}
	ann.u16()
	if ann.u16() != packetDeviceListAnnce {
		t.Fatal("no device list announce after logon")
	}
	if n := ann.u32(); n != 1 {
		t.Fatalf("announced %d devices, want 1", n)
	}
	if typ := ann.u32(); typ != deviceTypeFilesystem {
		t.Fatalf("device type %d, want filesystem", typ)
	}
	ann.u32()
	if dos := string(bytes.TrimRight(ann.take(8), "\x00")); dos != "mcp" {
		t.Fatalf("dos name %q, want mcp", dos)
	}

	d.Process(packet(componentCore, packetUserLoggedOn))
	if len(c.packets) != 0 {
		t.Fatal("drive announced twice")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := d.WaitAccepted(ctx); err == nil {
		t.Fatal("the drive reported accepted before the server replied")
	}
	d.Process(packet(componentCore, packetDeviceReply, le32(driveDeviceID), le32(0)))
	if err := d.WaitAccepted(context.Background()); err != nil {
		t.Fatal(err)
	}

	d.Process(packet(componentCore, packetServerAnnounce, []byte{1, 0, 0x0D, 0}, le32(8)))
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if err := d.WaitAccepted(ctx2); err == nil {
		t.Fatal("acceptance survived a new connection's server announce")
	}
}

type ioReply struct {
	status ntStatus
	body   *reader
}

func irp(d *Drive, c *capture, t *testing.T, fileID, major, minor uint32, body []byte) ioReply {
	t.Helper()
	d.Process(packet(componentCore, packetDeviceIORequest,
		le32(driveDeviceID), le32(fileID), le32(99), le32(major), le32(minor), body))
	r := &reader{buf: c.take(t)}
	r.u16()
	if r.u16() != packetDeviceIOResponse {
		t.Fatal("not an I/O completion")
	}
	r.u32()
	if id := r.u32(); id != 99 {
		t.Fatalf("completion id %d, want 99", id)
	}
	return ioReply{status: ntStatus(r.u32()), body: r}
}

func createBody(path string, access, disposition, options uint32) []byte {
	name := encodeUTF16(path, true)
	var w writer
	w.u32(access)
	w.u64(0)
	w.u32(0)
	w.u32(7)
	w.u32(disposition)
	w.u32(options)
	w.u32(uint32(len(name)))
	w.bytes(name)
	return w.buf
}

func open(t *testing.T, d *Drive, c *capture, path string, access, disposition, options uint32) (uint32, ntStatus) {
	t.Helper()
	rep := irp(d, c, t, 0, irpCreate, 0, createBody(path, access, disposition, options))
	return rep.body.u32(), rep.status
}

func TestCreateWriteReadRoundTrip(t *testing.T) {
	d, c, dir := newTestDrive(t)
	if err := os.Mkdir(filepath.Join(dir, "rpc"), 0o755); err != nil {
		t.Fatal(err)
	}

	id, status := open(t, d, c, `\rpc\req.json`, accessGenericWr, dispositionCreate, optionNonDirectoryFile)
	if status != statusSuccess {
		t.Fatalf("create: status 0x%08x", status)
	}

	payload := []byte(`{"hello":"world"}`)
	var w writer
	w.u32(uint32(len(payload)))
	w.u64(0)
	w.zeros(20)
	w.bytes(payload)
	rep := irp(d, c, t, id, irpWrite, 0, w.buf)
	if rep.status != statusSuccess || rep.body.u32() != uint32(len(payload)) {
		t.Fatalf("write: status 0x%08x", rep.status)
	}

	var rd writer
	rd.u32(1024)
	rd.u64(9)
	rd.zeros(20)
	rep = irp(d, c, t, id, irpRead, 0, rd.buf)
	n := rep.body.u32()
	if got := string(rep.body.take(int(n))); got != `"world"}` {
		t.Fatalf("read at offset 9 = %q", got)
	}

	var q writer
	q.u32(fileStandardInformation)
	q.u32(0)
	q.zeros(24)
	rep = irp(d, c, t, id, irpQueryInformation, 0, q.buf)
	rep.body.u32()
	rep.body.u64()
	if size := rep.body.u64(); size != uint64(len(payload)) {
		t.Fatalf("end of file %d, want %d", size, len(payload))
	}

	if rep := irp(d, c, t, id, irpClose, 0, make([]byte, 32)); rep.status != statusSuccess {
		t.Fatalf("close: status 0x%08x", rep.status)
	}
	got, err := os.ReadFile(filepath.Join(dir, "rpc", "req.json"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("file on disk = %q, %v", got, err)
	}
}

func TestCreateDispositions(t *testing.T) {
	d, c, dir := newTestDrive(t)
	if err := os.WriteFile(filepath.Join(dir, "exists"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path        string
		disposition uint32
		want        ntStatus
	}{
		{`\exists`, dispositionCreate, statusObjectNameCollision},
		{`\missing`, dispositionOpen, statusNoSuchFile},
		{`\nodir\missing`, dispositionOpen, statusObjectPathNotFound},
		{`\nodir\missing`, dispositionCreate, statusObjectPathNotFound},
		{`\missing`, dispositionOverwrite, statusNoSuchFile},
		{`\exists`, dispositionOpen, statusSuccess},
		{`\fresh`, dispositionOpenIf, statusSuccess},
	}
	for _, tc := range cases {
		if _, got := open(t, d, c, tc.path, accessGenericWr, tc.disposition, 0); got != tc.want {
			t.Errorf("%s disposition %d: status 0x%08x, want 0x%08x", tc.path, tc.disposition, got, tc.want)
		}
	}
}

func TestPathsCannotEscapeRoot(t *testing.T) {
	d, c, dir := newTestDrive(t)
	outside := filepath.Join(filepath.Dir(dir), "escaped")
	t.Cleanup(func() { os.Remove(outside) })

	id, status := open(t, d, c, `\..\..\escaped`, accessGenericWr, dispositionCreate, 0)
	if status != statusSuccess {
		t.Fatalf("create: status 0x%08x", status)
	}
	irp(d, c, t, id, irpClose, 0, make([]byte, 32))
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a .. path wrote outside the drive root")
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped")); err != nil {
		t.Fatalf("the .. path should resolve inside the root: %v", err)
	}
}

func renameBody(target string, replace bool) []byte {
	name := encodeUTF16(target, true)
	var info writer
	info.u8(map[bool]uint8{true: 1, false: 0}[replace])
	info.u8(0)
	info.u32(uint32(len(name)))
	info.bytes(name)
	var w writer
	w.u32(fileRenameInformation)
	w.u32(uint32(len(info.buf)))
	w.zeros(24)
	w.bytes(info.buf)
	return w.buf
}

func TestRenameReplacesOnlyWhenAsked(t *testing.T) {
	d, c, dir := newTestDrive(t)
	for name, body := range map[string]string{"a.tmp": "new", "a.json": "old"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	id, _ := open(t, d, c, `\a.tmp`, accessGenericWr, dispositionOpen, 0)

	if rep := irp(d, c, t, id, irpSetInformation, 0, renameBody(`\a.json`, false)); rep.status != statusObjectNameCollision {
		t.Fatalf("rename without replace: status 0x%08x", rep.status)
	}
	if rep := irp(d, c, t, id, irpSetInformation, 0, renameBody(`\a.json`, true)); rep.status != statusSuccess {
		t.Fatalf("rename with replace: status 0x%08x", rep.status)
	}
	irp(d, c, t, id, irpClose, 0, make([]byte, 32))

	got, err := os.ReadFile(filepath.Join(dir, "a.json"))
	if err != nil || string(got) != "new" {
		t.Fatalf("a.json = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.tmp")); err == nil {
		t.Fatal("a.tmp still exists after rename")
	}
}

func TestDeleteOnDisposition(t *testing.T) {
	d, c, dir := newTestDrive(t)
	if err := os.WriteFile(filepath.Join(dir, "gone"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "full", "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	disposition := func(pending bool) []byte {
		var w writer
		w.u32(fileDispositionInformation)
		w.u32(1)
		w.zeros(24)
		w.u8(map[bool]uint8{true: 1, false: 0}[pending])
		return w.buf
	}

	id, _ := open(t, d, c, `\gone`, accessDelete, dispositionOpen, 0)
	if rep := irp(d, c, t, id, irpSetInformation, 0, disposition(true)); rep.status != statusSuccess {
		t.Fatalf("disposition: status 0x%08x", rep.status)
	}
	irp(d, c, t, id, irpClose, 0, make([]byte, 32))
	if _, err := os.Stat(filepath.Join(dir, "gone")); err == nil {
		t.Fatal("file survived delete-on-close")
	}

	dirID, _ := open(t, d, c, `\full`, accessDelete, dispositionOpen, optionDirectoryFile)
	if rep := irp(d, c, t, dirID, irpSetInformation, 0, disposition(true)); rep.status != statusDirectoryNotEmpty {
		t.Fatalf("deleting a non-empty directory: status 0x%08x", rep.status)
	}
}

func TestQueryDirectoryFiltersAndOrders(t *testing.T) {
	d, c, dir := newTestDrive(t)
	for _, name := range []string{"b.json", "a.json", "c.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	id, _ := open(t, d, c, `\`, 0, dispositionOpen, optionDirectoryFile)

	query := func(initial bool, pattern string) ioReply {
		name := encodeUTF16(pattern, true)
		var w writer
		w.u32(fileNamesInformation)
		w.u8(map[bool]uint8{true: 1, false: 0}[initial])
		w.u32(uint32(len(name)))
		w.zeros(23)
		w.bytes(name)
		return irp(d, c, t, id, irpDirectoryControl, minorQueryDirectory, w.buf)
	}
	entryName := func(rep ioReply) string {
		rep.body.u32()
		rep.body.u32()
		rep.body.u32()
		n := rep.body.u32()
		return decodeUTF16(rep.body.take(int(n)))
	}

	var got []string
	rep := query(true, `\*.JSON`)
	for rep.status == statusSuccess {
		got = append(got, entryName(rep))
		rep = query(false, "")
	}
	if rep.status != statusNoMoreFiles {
		t.Fatalf("end of listing: status 0x%08x", rep.status)
	}
	if len(got) != 2 || got[0] != "a.json" || got[1] != "b.json" {
		t.Fatalf("listing = %v, want [a.json b.json]", got)
	}

	if rep := query(true, `\nothing*`); rep.status != statusNoSuchFile {
		t.Fatalf("empty initial query: status 0x%08x", rep.status)
	}
}

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything", true},
		{"*.json", "a.json", true},
		{"*.json", "a.json.tmp", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"<.json", "x.JSON", true},
		{"ready", "READY", true},
		{"*a*b", "xxaxxb", true},
		{"*a*b", "xxaxxbc", false},
	}
	for _, tc := range cases {
		if got := wildcardMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestNewDriveRejectsBadNames(t *testing.T) {
	for _, name := range []string{"", "toolongname", `a\b`, "a b", "c:"} {
		if _, err := NewDrive(name, t.TempDir()); err == nil {
			t.Errorf("NewDrive(%q) accepted an invalid name", name)
		}
	}
}
