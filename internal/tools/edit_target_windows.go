package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsEditDirectory struct{ file *os.File }

func captureEditTarget(path string, whitelist PathWhitelist) (editTarget, error) {
	return capturePortableEditTarget(path, whitelist, openWindowsEditDirectory)
}

func windowsEditComponent(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `\/:`) {
		return errors.New("edit: invalid Windows path component")
	}
	return nil
}

func windowsEditError(err error) error {
	if status, ok := err.(windows.NTStatus); ok {
		return status.Errno()
	}
	return err
}

// windowsEditOpenRelative opens exactly one component without following its
// reparse point. The next component is opened only after this handle is proven
// to be a real directory.
func windowsEditOpenRelative(parent windows.Handle, name string, access, disposition, options uint32) (windows.Handle, error) {
	if err := windowsEditComponent(name); err != nil {
		return 0, err
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, access|windows.SYNCHRONIZE|windows.FILE_READ_ATTRIBUTES,
		&attributes, &status, nil, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		disposition, options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0)
	if err != nil {
		return 0, windowsEditError(err)
	}
	return handle, nil
}

func windowsEditCheckHandle(handle windows.Handle, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("edit: reparse point is not allowed")
	}
	isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != directory {
		return errors.New("edit: unexpected directory or non-regular target")
	}
	kind, err := windows.GetFileType(handle)
	if err != nil || kind != windows.FILE_TYPE_DISK {
		return errors.Join(errors.New("edit: non-disk target is not allowed"), err)
	}
	return nil
}

func openWindowsEditDirectory(path string) (portableEditDirectory, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("edit: directory is not absolute: %q", path)
	}
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	if volume == "" {
		return nil, errors.New("edit: Windows volume root is unavailable")
	}
	root := volume + string(filepath.Separator)
	rootName, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(rootName, windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, fmt.Errorf("edit: open volume root: %w", err)
	}
	if err := windowsEditCheckHandle(handle, true); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("edit: verify volume root: %w", err)
	}
	remainder := strings.TrimLeft(strings.TrimPrefix(path, volume), `\/`)
	for _, component := range strings.FieldsFunc(remainder, func(r rune) bool { return r == '\\' || r == '/' }) {
		next, openErr := windowsEditOpenRelative(handle, component, windows.FILE_TRAVERSE, windows.FILE_OPEN, 0)
		_ = windows.CloseHandle(handle)
		if openErr != nil {
			return nil, fmt.Errorf("edit: open directory component: %w", openErr)
		}
		if checkErr := windowsEditCheckHandle(next, true); checkErr != nil {
			_ = windows.CloseHandle(next)
			return nil, fmt.Errorf("edit: verify directory component: %w", checkErr)
		}
		handle = next
	}
	return &windowsEditDirectory{file: os.NewFile(uintptr(handle), path)}, nil
}

func (d *windowsEditDirectory) Stat() (os.FileInfo, error) { return d.file.Stat() }
func (d *windowsEditDirectory) Close() error               { return d.file.Close() }

func (d *windowsEditDirectory) OpenLeaf(name string) (*os.File, error) {
	handle, err := windowsEditOpenRelative(windows.Handle(d.file.Fd()), name, windows.FILE_GENERIC_READ, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE)
	if err != nil {
		return nil, err
	}
	if err := windowsEditCheckHandle(handle, false); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	return os.NewFile(uintptr(handle), name), nil
}

func (d *windowsEditDirectory) CreateLeaf(name string) (*os.File, error) {
	handle, err := windowsEditOpenRelative(windows.Handle(d.file.Fd()), name, windows.FILE_GENERIC_WRITE|windows.DELETE, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE)
	if err != nil {
		return nil, err
	}
	if err := windowsEditCheckHandle(handle, false); err != nil {
		deleteFile := byte(1)
		var status windows.IO_STATUS_BLOCK
		cleanupErr := windows.NtSetInformationFile(handle, &status, &deleteFile, 1, windows.FileDispositionInformation)
		_ = windows.CloseHandle(handle)
		return nil, errors.Join(err, windowsEditError(cleanupErr))
	}
	return os.NewFile(uintptr(handle), name), nil
}

type windowsEditRenameInformation struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

func (d *windowsEditDirectory) RenameLeaf(file *os.File, _, name string) error {
	if err := windowsEditComponent(name); err != nil {
		return err
	}
	nameUTF16, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	nameUTF16 = nameUTF16[:len(nameUTF16)-1]
	var layout windowsEditRenameInformation
	size := max(int(unsafe.Sizeof(layout)), int(unsafe.Offsetof(layout.FileName))+len(nameUTF16)*2)
	buffer := make([]byte, size)
	info := (*windowsEditRenameInformation)(unsafe.Pointer(&buffer[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.RootDirectory = windows.Handle(d.file.Fd())
	info.FileNameLength = uint32(len(nameUTF16) * 2)
	copy(unsafe.Slice(&info.FileName[0], len(nameUTF16)), nameUTF16)
	var status windows.IO_STATUS_BLOCK
	const fileRenameInformationEx = 65
	err = windows.NtSetInformationFile(windows.Handle(file.Fd()), &status, &buffer[0], uint32(len(buffer)), fileRenameInformationEx)
	if errors.Is(err, windows.STATUS_INVALID_INFO_CLASS) || errors.Is(err, windows.STATUS_INVALID_PARAMETER) || errors.Is(err, windows.STATUS_NOT_SUPPORTED) {
		// The old information class is still handle-relative. It may safely fail
		// when the filesystem cannot replace a target with an open handle.
		info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS
		err = windows.NtSetInformationFile(windows.Handle(file.Fd()), &status, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation)
	}
	return windowsEditError(err)
}

func (d *windowsEditDirectory) RemoveLeaf(file *os.File, _ string) error {
	// Delete the captured temporary object by handle, never a substituted path.
	_ = file.Chmod(0o600)
	deleteFile := byte(1)
	var status windows.IO_STATUS_BLOCK
	return windowsEditError(windows.NtSetInformationFile(windows.Handle(file.Fd()), &status, &deleteFile, 1, windows.FileDispositionInformation))
}
