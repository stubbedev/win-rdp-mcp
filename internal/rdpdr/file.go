package rdpdr

import (
	"errors"
	"io/fs"
	"os"
)

type openFile struct {
	root          *os.Root
	rel           string
	f             *os.File
	reopenFlag    int
	dir           bool
	deleteOnClose bool

	listing []listedEntry
	next    int
}

func (o *openFile) stat() (fs.FileInfo, error) {
	if o.f != nil {
		return o.f.Stat()
	}
	return o.root.Stat(o.rel)
}

func (o *openFile) close() error {
	var closeErr error
	if o.f != nil {
		closeErr = o.f.Close()
		o.f = nil
	}
	if !o.deleteOnClose {
		return closeErr
	}
	if err := o.root.Remove(o.rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(closeErr, err)
	}
	return closeErr
}

func (o *openFile) setTimes(atimeFT, mtimeFT uint64) ntStatus {
	atime, setA := fromFiletime(atimeFT)
	mtime, setM := fromFiletime(mtimeFT)
	if !setA && !setM {
		return statusSuccess
	}
	fi, err := o.stat()
	if err != nil {
		return statusFor(err)
	}
	if !setM {
		mtime = fi.ModTime()
	}
	if !setA {
		atime = mtime
	}
	if err := o.root.Chtimes(o.rel, atime, mtime); err != nil {
		return statusFor(err)
	}
	return statusSuccess
}

func (o *openFile) truncate(size int64, exact bool) ntStatus {
	if o.f == nil {
		return statusInvalidParameter
	}
	if !exact {
		fi, err := o.f.Stat()
		if err != nil {
			return statusFor(err)
		}
		if size >= fi.Size() {
			return statusSuccess
		}
	}
	if err := o.f.Truncate(size); err != nil {
		return statusFor(err)
	}
	return statusSuccess
}

func (o *openFile) setDeletePending(pending bool) ntStatus {
	if pending && o.dir {
		h, err := o.root.Open(o.rel)
		if err != nil {
			return statusFor(err)
		}
		names, err := h.Readdirnames(1)
		h.Close()
		if err == nil && len(names) > 0 {
			return statusDirectoryNotEmpty
		}
	}
	o.deleteOnClose = pending
	return statusSuccess
}

func (o *openFile) rename(target string, replace bool) ntStatus {
	if target == "." {
		return statusInvalidParameter
	}
	if _, err := o.root.Lstat(target); err == nil {
		if !replace {
			return statusObjectNameCollision
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return statusFor(err)
	}
	if o.f != nil {
		if err := o.f.Close(); err != nil {
			return statusFor(err)
		}
		o.f = nil
	}
	renameErr := o.root.Rename(o.rel, target)
	if renameErr == nil {
		o.rel = target
	}
	if !o.dir {
		f, err := o.root.OpenFile(o.rel, o.reopenFlag, 0)
		if err != nil {
			return statusFor(errors.Join(renameErr, err))
		}
		o.f = f
	}
	if renameErr != nil {
		return statusFor(renameErr)
	}
	return statusSuccess
}
