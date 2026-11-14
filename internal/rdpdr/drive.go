package rdpdr

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/stubbedev/win-rdp-mcp/internal/grdp/core"
	"github.com/stubbedev/win-rdp-mcp/internal/grdp/plugin"
)

const (
	componentCore    = 0x4472
	componentPrinter = 0x5052

	packetServerAnnounce   = 0x496E
	packetClientIDConfirm  = 0x4343
	packetClientName       = 0x434E
	packetServerCapability = 0x5350
	packetClientCapability = 0x4350
	packetDeviceListAnnce  = 0x4441
	packetDeviceReply      = 0x6472
	packetDeviceIORequest  = 0x4952
	packetDeviceIOResponse = 0x4943
	packetUserLoggedOn     = 0x554C

	capGeneral = 1
	capDrive   = 4

	generalCapabilityVersion2 = 2
	driveCapabilityVersion2   = 2

	extendedDeviceRemove  = 0x1
	extendedDisplayName   = 0x2
	extendedUserLoggedOn  = 0x4
	clientProtocolMinor   = 0x000C
	serverMinorAnnounceOK = 0x0005

	deviceTypeFilesystem = 0x8
	driveDeviceID        = 1
	clientComputerName   = "win-rdp-mcp"
	maxReadLength        = 16 << 20
)

const (
	irpCreate           = 0x00
	irpClose            = 0x02
	irpRead             = 0x03
	irpWrite            = 0x04
	irpQueryInformation = 0x05
	irpSetInformation   = 0x06
	irpQueryVolumeInfo  = 0x0A
	irpDirectoryControl = 0x0C
	irpDeviceControl    = 0x0E
	irpLockControl      = 0x11

	minorQueryDirectory        = 0x01
	minorNotifyChangeDirectory = 0x02
)

type Drive struct {
	name string
	dir  string

	mu          sync.Mutex
	root        *os.Root
	sender      core.ChannelSender
	serverMinor uint16
	announced   bool
	nextFileID  uint32
	files       map[uint32]*openFile
}

func NewDrive(name, dir string) (*Drive, error) {
	if name == "" || len(name) > 7 {
		return nil, fmt.Errorf("drive name %q must be 1 to 7 characters", name)
	}
	for _, r := range name {
		if r > 0x7E || r <= 0x20 || strings.ContainsRune(`:<>"/\|?*`, r) {
			return nil, fmt.Errorf("drive name %q must be printable ASCII without path or wildcard characters", name)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("opening redirected drive directory %s: %w", dir, err)
	}
	return &Drive{name: name, dir: dir, root: root, files: map[uint32]*openFile{}}, nil
}

func (d *Drive) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeAllLocked()
	return d.root.Close()
}

func (d *Drive) GetType() (string, uint32) {
	return plugin.RDPDR_SVC_CHANNEL_NAME,
		plugin.CHANNEL_OPTION_INITIALIZED | plugin.CHANNEL_OPTION_ENCRYPT_RDP | plugin.CHANNEL_OPTION_COMPRESS_RDP
}

func (d *Drive) Sender(s core.ChannelSender) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sender = s
}

func (d *Drive) Process(data []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.processLocked(data); err != nil {
		slog.Warn("rdpdr: dropping server packet", "err", err)
	}
}

func (d *Drive) send(w writer) error {
	if d.sender == nil {
		return errors.New("rdpdr: no channel sender attached")
	}
	if _, err := d.sender.SendToChannel(plugin.RDPDR_SVC_CHANNEL_NAME, w.buf); err != nil {
		return fmt.Errorf("rdpdr: sending to channel: %w", err)
	}
	return nil
}

func (d *Drive) processLocked(data []byte) error {
	r := &reader{buf: data}
	component, packetID := r.u16(), r.u16()
	if r.err != nil {
		return &packetError{"header", r.err}
	}
	if component == componentPrinter {
		return nil
	}
	if component != componentCore {
		return fmt.Errorf("unknown component 0x%04x", component)
	}
	switch packetID {
	case packetServerAnnounce:
		return d.onServerAnnounce(r)
	case packetServerCapability:
		return d.sendClientCapability()
	case packetClientIDConfirm:
		if d.serverMinor == serverMinorAnnounceOK {
			return d.announceDevices()
		}
		return nil
	case packetUserLoggedOn:
		return d.announceDevices()
	case packetDeviceReply:
		return d.onDeviceReply(r)
	case packetDeviceIORequest:
		return d.onIORequest(r)
	}
	return nil
}

func (d *Drive) onServerAnnounce(r *reader) error {
	r.u16()
	minor := r.u16()
	clientID := r.u32()
	if r.err != nil {
		return &packetError{"server announce", r.err}
	}
	d.closeAllLocked()
	d.serverMinor = minor
	d.announced = false

	confirm := header(componentCore, packetClientIDConfirm)
	confirm.u16(1)
	confirm.u16(clientProtocolMinor)
	confirm.u32(clientID)
	if err := d.send(confirm); err != nil {
		return err
	}

	nameBytes := encodeUTF16(clientComputerName, true)
	name := header(componentCore, packetClientName)
	name.u32(1)
	name.u32(0)
	name.u32(uint32(len(nameBytes)))
	name.bytes(nameBytes)
	return d.send(name)
}

func (d *Drive) sendClientCapability() error {
	w := header(componentCore, packetClientCapability)
	w.u16(2)
	w.u16(0)

	w.u16(capGeneral)
	w.u16(44)
	w.u32(generalCapabilityVersion2)
	w.u32(0)
	w.u32(0)
	w.u16(1)
	w.u16(clientProtocolMinor)
	w.u32(0xFFFF)
	w.u32(0)
	w.u32(extendedDeviceRemove | extendedDisplayName | extendedUserLoggedOn)
	w.u32(0)
	w.u32(0)
	w.u32(0)

	w.u16(capDrive)
	w.u16(8)
	w.u32(driveCapabilityVersion2)
	return d.send(w)
}

func (d *Drive) announceDevices() error {
	if d.announced {
		return nil
	}
	d.announced = true
	w := header(componentCore, packetDeviceListAnnce)
	w.u32(1)
	w.u32(deviceTypeFilesystem)
	w.u32(driveDeviceID)
	dos := make([]byte, 8)
	copy(dos, d.name)
	w.bytes(dos)
	w.u32(uint32(len(d.name) + 1))
	w.bytes([]byte(d.name))
	w.u8(0)
	return d.send(w)
}

func (d *Drive) onDeviceReply(r *reader) error {
	deviceID, result := r.u32(), r.u32()
	if r.err != nil {
		return &packetError{"device reply", r.err}
	}
	if result != 0 {
		return fmt.Errorf("server rejected redirected drive %q (device %d): status 0x%08x", d.name, deviceID, result)
	}
	return nil
}

func (d *Drive) closeAllLocked() {
	for id, f := range d.files {
		f.close()
		delete(d.files, id)
	}
}

type ioRequest struct {
	deviceID, fileID, completionID uint32
	major, minor                   uint32
}

func (req ioRequest) reply(status ntStatus) writer {
	w := header(componentCore, packetDeviceIOResponse)
	w.u32(req.deviceID)
	w.u32(req.completionID)
	w.u32(uint32(status))
	return w
}

func (d *Drive) onIORequest(r *reader) error {
	req := ioRequest{deviceID: r.u32(), fileID: r.u32(), completionID: r.u32(), major: r.u32(), minor: r.u32()}
	if r.err != nil {
		return &packetError{"device I/O request", r.err}
	}
	if req.deviceID != driveDeviceID {
		return d.send(req.reply(statusNoSuchDevice))
	}
	var resp writer
	var err error
	switch req.major {
	case irpCreate:
		resp, err = d.create(req, r)
	case irpClose:
		resp = d.closeFile(req)
	case irpRead:
		resp, err = d.read(req, r)
	case irpWrite:
		resp, err = d.write(req, r)
	case irpQueryInformation:
		resp, err = d.queryInformation(req, r)
	case irpSetInformation:
		resp, err = d.setInformation(req, r)
	case irpQueryVolumeInfo:
		resp, err = d.queryVolume(req, r)
	case irpDirectoryControl:
		if req.minor == minorNotifyChangeDirectory {
			return nil
		}
		if req.minor != minorQueryDirectory {
			resp = req.reply(statusNotSupported)
			resp.u32(0)
			break
		}
		resp, err = d.queryDirectory(req, r)
	case irpDeviceControl:
		resp = req.reply(statusSuccess)
		resp.u32(0)
	case irpLockControl:
		resp = req.reply(statusSuccess)
		resp.zeros(5)
	default:
		resp = req.reply(statusNotSupported)
	}
	if err != nil {
		return err
	}
	return d.send(resp)
}

func (d *Drive) file(id uint32) (*openFile, bool) {
	f, ok := d.files[id]
	return f, ok
}

func cleanPath(p string) string {
	p = strings.TrimRight(p, "\x00")
	p = path.Clean("/" + strings.ReplaceAll(p, `\`, "/"))
	if p == "/" {
		return "."
	}
	return p[1:]
}

func (d *Drive) parentMissing(rel string) bool {
	parent := path.Dir(rel)
	if parent == "." {
		return false
	}
	fi, err := d.root.Stat(parent)
	return err != nil || !fi.IsDir()
}

func (d *Drive) notFound(rel string) ntStatus {
	if d.parentMissing(rel) {
		return statusObjectPathNotFound
	}
	return statusNoSuchFile
}

const (
	dispositionSupersede   = 0
	dispositionOpen        = 1
	dispositionCreate      = 2
	dispositionOpenIf      = 3
	dispositionOverwrite   = 4
	dispositionOverwriteIf = 5

	optionDirectoryFile    = 0x00000001
	optionNonDirectoryFile = 0x00000040
	optionDeleteOnClose    = 0x00001000

	accessWriteData  = 0x00000002
	accessAppendData = 0x00000004
	accessMaximum    = 0x02000000
	accessGenericAll = 0x10000000
	accessGenericWr  = 0x40000000
)

const (
	infoSuperseded uint8 = iota
	infoOpened
	infoCreated
	infoOverwritten
)

func wantsWrite(access uint32) bool {
	return access&(accessWriteData|accessAppendData|accessGenericWr|accessGenericAll|accessMaximum) != 0
}

func (d *Drive) create(req ioRequest, r *reader) (writer, error) {
	access := r.u32()
	r.skip(8)
	r.u32()
	r.u32()
	disposition := r.u32()
	options := r.u32()
	pathLen := r.u32()
	rel := cleanPath(r.utf16z(int(pathLen)))
	if r.err != nil {
		return writer{}, &packetError{"create request", r.err}
	}

	f, info, status := d.open(rel, access, disposition, options)
	resp := req.reply(status)
	if status != statusSuccess {
		resp.u32(0)
		resp.u8(0)
		return resp, nil
	}
	d.nextFileID++
	id := d.nextFileID
	d.files[id] = f
	resp.u32(id)
	resp.u8(info)
	return resp, nil
}

func (d *Drive) open(rel string, access, disposition, options uint32) (*openFile, uint8, ntStatus) {
	fi, statErr := d.root.Stat(rel)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return nil, 0, statusFor(statErr)
	}
	deleteOnClose := options&optionDeleteOnClose != 0

	if exists && fi.IsDir() {
		if options&optionNonDirectoryFile != 0 {
			return nil, 0, statusFileIsADirectory
		}
		if disposition == dispositionCreate {
			return nil, 0, statusObjectNameCollision
		}
		return &openFile{root: d.root, rel: rel, dir: true, deleteOnClose: deleteOnClose}, infoOpened, statusSuccess
	}

	if options&optionDirectoryFile != 0 {
		if exists {
			return nil, 0, statusNotADirectory
		}
		if disposition != dispositionCreate && disposition != dispositionOpenIf {
			return nil, 0, d.notFound(rel)
		}
		if err := d.root.Mkdir(rel, 0o755); err != nil {
			return nil, 0, d.statusForCreate(rel, err)
		}
		return &openFile{root: d.root, rel: rel, dir: true, deleteOnClose: deleteOnClose}, infoCreated, statusSuccess
	}

	flag := os.O_RDONLY
	if wantsWrite(access) {
		flag = os.O_RDWR
	}
	var info uint8
	switch disposition {
	case dispositionSupersede:
		flag, info = os.O_RDWR|os.O_CREATE|os.O_TRUNC, pick(exists, infoSuperseded, infoCreated)
	case dispositionOpen:
		if !exists {
			return nil, 0, d.notFound(rel)
		}
		info = infoOpened
	case dispositionCreate:
		if exists {
			return nil, 0, statusObjectNameCollision
		}
		flag, info = os.O_RDWR|os.O_CREATE|os.O_EXCL, infoCreated
	case dispositionOpenIf:
		if !exists {
			flag |= os.O_CREATE
			if flag&os.O_RDWR == 0 {
				flag = os.O_RDWR | os.O_CREATE
			}
		}
		info = pick(exists, infoOpened, infoCreated)
	case dispositionOverwrite:
		if !exists {
			return nil, 0, d.notFound(rel)
		}
		flag, info = os.O_RDWR|os.O_TRUNC, infoOverwritten
	case dispositionOverwriteIf:
		flag, info = os.O_RDWR|os.O_CREATE|os.O_TRUNC, pick(exists, infoOverwritten, infoCreated)
	default:
		return nil, 0, statusInvalidParameter
	}

	f, err := d.root.OpenFile(rel, flag, 0o644)
	if err != nil {
		return nil, 0, d.statusForCreate(rel, err)
	}
	reopen := flag &^ (os.O_CREATE | os.O_EXCL | os.O_TRUNC)
	return &openFile{root: d.root, rel: rel, f: f, reopenFlag: reopen, deleteOnClose: deleteOnClose}, info, statusSuccess
}

func (d *Drive) statusForCreate(rel string, err error) ntStatus {
	if errors.Is(err, fs.ErrNotExist) {
		return d.notFound(rel)
	}
	return statusFor(err)
}

func pick[T any](cond bool, yes, no T) T {
	if cond {
		return yes
	}
	return no
}

func (d *Drive) closeFile(req ioRequest) writer {
	resp := req.reply(statusSuccess)
	resp.zeros(5)
	f, ok := d.file(req.fileID)
	if !ok {
		resp = req.reply(statusInvalidHandle)
		resp.zeros(5)
		return resp
	}
	delete(d.files, req.fileID)
	if err := f.close(); err != nil {
		slog.Warn("rdpdr: closing redirected file", "path", f.rel, "err", err)
		resp = req.reply(statusFor(err))
		resp.zeros(5)
	}
	return resp
}

func (d *Drive) read(req ioRequest, r *reader) (writer, error) {
	length := r.u32()
	offset := r.u64()
	if r.err != nil {
		return writer{}, &packetError{"read request", r.err}
	}
	f, ok := d.file(req.fileID)
	if !ok || f.f == nil {
		resp := req.reply(pick(ok, statusInvalidDeviceRequest, statusInvalidHandle))
		resp.u32(0)
		return resp, nil
	}
	buf := make([]byte, min(length, maxReadLength))
	n, err := f.f.ReadAt(buf, int64(offset))
	if err != nil && !errors.Is(err, io.EOF) {
		resp := req.reply(statusFor(err))
		resp.u32(0)
		return resp, nil
	}
	resp := req.reply(statusSuccess)
	resp.u32(uint32(n))
	resp.bytes(buf[:n])
	return resp, nil
}

func (d *Drive) write(req ioRequest, r *reader) (writer, error) {
	length := r.u32()
	offset := r.u64()
	r.skip(20)
	data := r.take(int(length))
	if r.err != nil {
		return writer{}, &packetError{"write request", r.err}
	}
	f, ok := d.file(req.fileID)
	if !ok || f.f == nil {
		resp := req.reply(pick(ok, statusInvalidDeviceRequest, statusInvalidHandle))
		resp.u32(0)
		resp.u8(0)
		return resp, nil
	}
	n, err := f.f.WriteAt(data, int64(offset))
	status := statusSuccess
	if err != nil {
		status = statusFor(err)
	}
	resp := req.reply(status)
	resp.u32(uint32(n))
	resp.u8(0)
	return resp, nil
}

const (
	fileBasicInformation         = 4
	fileStandardInformation      = 5
	fileRenameInformation        = 10
	fileDispositionInformation   = 13
	fileAllocationInformation    = 19
	fileEndOfFileInformation     = 20
	fileAttributeTagInformation  = 35
	fileDirectoryInformation     = 1
	fileFullDirectoryInformation = 2
	fileBothDirectoryInformation = 3
	fileNamesInformation         = 12

	attributeReadonly  = 0x00000001
	attributeDirectory = 0x00000010
	attributeArchive   = 0x00000020
)

func attributes(fi fs.FileInfo) uint32 {
	var a uint32 = attributeArchive
	if fi.IsDir() {
		a = attributeDirectory
	}
	if fi.Mode().Perm()&0o200 == 0 {
		a |= attributeReadonly
	}
	return a
}

func (d *Drive) queryInformation(req ioRequest, r *reader) (writer, error) {
	class := r.u32()
	if r.err != nil {
		return writer{}, &packetError{"query information request", r.err}
	}
	f, ok := d.file(req.fileID)
	if !ok {
		resp := req.reply(statusInvalidHandle)
		resp.u32(0)
		return resp, nil
	}
	fi, err := f.stat()
	if err != nil {
		resp := req.reply(statusFor(err))
		resp.u32(0)
		return resp, nil
	}
	var body writer
	mtime := filetime(fi.ModTime())
	switch class {
	case fileBasicInformation:
		body.u64(mtime)
		body.u64(mtime)
		body.u64(mtime)
		body.u64(mtime)
		body.u32(attributes(fi))
	case fileStandardInformation:
		body.u64(uint64(fi.Size()))
		body.u64(uint64(fi.Size()))
		body.u32(1)
		body.u8(pick[uint8](f.deleteOnClose, 1, 0))
		body.u8(pick[uint8](fi.IsDir(), 1, 0))
	case fileAttributeTagInformation:
		body.u32(attributes(fi))
		body.u32(0)
	default:
		resp := req.reply(statusInvalidInfoClass)
		resp.u32(0)
		return resp, nil
	}
	resp := req.reply(statusSuccess)
	resp.u32(uint32(len(body.buf)))
	resp.bytes(body.buf)
	return resp, nil
}

func (d *Drive) setInformation(req ioRequest, r *reader) (writer, error) {
	class := r.u32()
	length := r.u32()
	r.skip(24)
	body := &reader{buf: r.take(int(length))}
	if r.err != nil {
		return writer{}, &packetError{"set information request", r.err}
	}
	reply := func(status ntStatus) writer {
		resp := req.reply(status)
		resp.u32(length)
		return resp
	}
	f, ok := d.file(req.fileID)
	if !ok {
		return reply(statusInvalidHandle), nil
	}
	switch class {
	case fileBasicInformation:
		body.u64()
		atime, mtime := body.u64(), body.u64()
		if body.err != nil {
			return writer{}, &packetError{"basic information", body.err}
		}
		return reply(f.setTimes(atime, mtime)), nil
	case fileEndOfFileInformation:
		size := body.u64()
		if body.err != nil {
			return writer{}, &packetError{"end-of-file information", body.err}
		}
		return reply(f.truncate(int64(size), true)), nil
	case fileAllocationInformation:
		size := body.u64()
		if body.err != nil {
			return writer{}, &packetError{"allocation information", body.err}
		}
		return reply(f.truncate(int64(size), false)), nil
	case fileDispositionInformation:
		pending := length == 0 || body.u8() != 0
		return reply(f.setDeletePending(pending)), nil
	case fileRenameInformation:
		replace := body.u8() != 0
		body.u8()
		nameLen := body.u32()
		target := cleanPath(body.utf16z(int(nameLen)))
		if body.err != nil {
			return writer{}, &packetError{"rename information", body.err}
		}
		return reply(f.rename(target, replace)), nil
	}
	return reply(statusInvalidInfoClass), nil
}

const (
	fileFsVolumeInformation    = 1
	fileFsSizeInformation      = 3
	fileFsDeviceInformation    = 4
	fileFsAttributeInformation = 5
	fileFsFullSizeInformation  = 7

	fileDeviceDisk            = 0x00000007
	fsCaseSensitiveSearch     = 0x00000001
	fsCasePreservedNames      = 0x00000002
	fsUnicodeOnDisk           = 0x00000004
	maxComponentNameLength    = 255
	bytesPerSector            = 512
	volumeSerial              = 0x4D435000
	reportedFileSystemName    = "FAT32"
	sectorsPerAllocationUnits = 8
)

func (d *Drive) queryVolume(req ioRequest, r *reader) (writer, error) {
	class := r.u32()
	if r.err != nil {
		return writer{}, &packetError{"query volume request", r.err}
	}
	var body writer
	switch class {
	case fileFsVolumeInformation:
		label := encodeUTF16(d.name, true)
		body.u64(0)
		body.u32(volumeSerial)
		body.u32(uint32(len(label)))
		body.u8(0)
		body.bytes(label)
	case fileFsSizeInformation, fileFsFullSizeInformation:
		total, free, err := diskSpace(d.dir)
		if err != nil {
			resp := req.reply(statusFor(err))
			resp.u32(0)
			return resp, nil
		}
		unit := uint64(bytesPerSector * sectorsPerAllocationUnits)
		body.u64(total / unit)
		body.u64(free / unit)
		if class == fileFsFullSizeInformation {
			body.u64(free / unit)
		}
		body.u32(sectorsPerAllocationUnits)
		body.u32(bytesPerSector)
	case fileFsAttributeInformation:
		name := encodeUTF16(reportedFileSystemName, false)
		body.u32(fsCaseSensitiveSearch | fsCasePreservedNames | fsUnicodeOnDisk)
		body.u32(maxComponentNameLength)
		body.u32(uint32(len(name)))
		body.bytes(name)
	case fileFsDeviceInformation:
		body.u32(fileDeviceDisk)
		body.u32(0)
	default:
		resp := req.reply(statusInvalidInfoClass)
		resp.u32(0)
		return resp, nil
	}
	resp := req.reply(statusSuccess)
	resp.u32(uint32(len(body.buf)))
	resp.bytes(body.buf)
	return resp, nil
}

func (d *Drive) queryDirectory(req ioRequest, r *reader) (writer, error) {
	class := r.u32()
	initial := r.u8() != 0
	pathLen := r.u32()
	r.skip(23)
	query := r.utf16z(int(pathLen))
	if r.err != nil {
		return writer{}, &packetError{"query directory request", r.err}
	}
	empty := func(status ntStatus) writer {
		resp := req.reply(status)
		resp.u32(0)
		resp.u8(0)
		return resp
	}
	f, ok := d.file(req.fileID)
	if !ok {
		return empty(statusInvalidHandle), nil
	}
	if !slices.Contains([]uint32{fileDirectoryInformation, fileFullDirectoryInformation,
		fileBothDirectoryInformation, fileNamesInformation}, class) {
		return empty(statusInvalidInfoClass), nil
	}
	if initial {
		entries, status := d.list(query)
		if status != statusSuccess {
			return empty(status), nil
		}
		f.listing, f.next = entries, 0
		if len(entries) == 0 {
			return empty(statusNoSuchFile), nil
		}
	}
	if f.next >= len(f.listing) {
		return empty(statusNoMoreFiles), nil
	}
	entry := f.listing[f.next]
	f.next++
	body := directoryEntry(class, entry)
	resp := req.reply(statusSuccess)
	resp.u32(uint32(len(body)))
	resp.bytes(body)
	return resp, nil
}

type listedEntry struct {
	name string
	info fs.FileInfo
}

func (d *Drive) list(query string) ([]listedEntry, ntStatus) {
	clean := strings.ReplaceAll(strings.TrimRight(query, "\x00"), `\`, "/")
	dirPart, pattern := path.Split(clean)
	dir := cleanPath(dirPart)
	if pattern == "" {
		pattern = "*"
	}
	h, err := d.root.Open(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, statusObjectPathNotFound
		}
		return nil, statusFor(err)
	}
	defer h.Close()
	dirEntries, err := h.ReadDir(-1)
	if err != nil {
		return nil, statusFor(err)
	}
	out := make([]listedEntry, 0, len(dirEntries))
	for _, e := range dirEntries {
		if !wildcardMatch(pattern, e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, statusFor(err)
		}
		out = append(out, listedEntry{name: e.Name(), info: info})
	}
	slices.SortFunc(out, func(a, b listedEntry) int { return cmp.Compare(a.name, b.name) })
	return out, statusSuccess
}

func directoryEntry(class uint32, e listedEntry) []byte {
	name := encodeUTF16(e.name, false)
	mtime := filetime(e.info.ModTime())
	size := uint64(e.info.Size())
	var w writer
	w.u32(0)
	w.u32(0)
	if class == fileNamesInformation {
		w.u32(uint32(len(name)))
		w.bytes(name)
		return w.buf
	}
	w.u64(mtime)
	w.u64(mtime)
	w.u64(mtime)
	w.u64(mtime)
	w.u64(size)
	w.u64(size)
	w.u32(attributes(e.info))
	w.u32(uint32(len(name)))
	switch class {
	case fileFullDirectoryInformation:
		w.u32(0)
	case fileBothDirectoryInformation:
		w.u32(0)
		w.u8(0)
		w.u8(0)
		w.zeros(24)
	}
	w.bytes(name)
	return w.buf
}

func wildcardMatch(pattern, name string) bool {
	p := []rune(strings.ToLower(pattern))
	n := []rune(strings.ToLower(name))
	for i, c := range p {
		switch c {
		case '<':
			p[i] = '*'
		case '>':
			p[i] = '?'
		case '"':
			p[i] = '.'
		}
	}
	return matchRunes(p, n)
}

func matchRunes(p, n []rune) bool {
	star, starN := -1, 0
	i, j := 0, 0
	for j < len(n) {
		switch {
		case i < len(p) && (p[i] == '?' || p[i] == n[j]):
			i++
			j++
		case i < len(p) && p[i] == '*':
			star, starN = i, j
			i++
		case star >= 0:
			i = star + 1
			starN++
			j = starN
		default:
			return false
		}
	}
	for i < len(p) && p[i] == '*' {
		i++
	}
	return i == len(p)
}
