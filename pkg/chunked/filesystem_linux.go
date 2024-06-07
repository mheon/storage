package chunked

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	driversCopy "github.com/containers/storage/drivers/copy"
	"github.com/containers/storage/pkg/archive"
	"github.com/containers/storage/pkg/chunked/internal"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/vbatts/tar-split/archive/tar"
	"golang.org/x/sys/unix"
)

// procPathForFile returns an absolute path in /proc which
// refers to the file; see procPathForFd.
func procPathForFile(f *os.File) string {
	return procPathForFd(int(f.Fd()))
}

// procPathForFd returns an absolute path in /proc which
// refers to the file; this allows passing a file descriptor
// in places that don't accept a file descriptor.
func procPathForFd(fd int) string {
	return fmt.Sprintf("/proc/self/fd/%d", fd)
}

// fileMetadata is a wrapper around internal.FileMetadata with additional private fields that
// are not part of the TOC document.
// Type: TypeChunk entries are stored in Chunks, the primary [fileMetadata] entries never use TypeChunk.
type fileMetadata struct {
	internal.FileMetadata

	// chunks stores the TypeChunk entries relevant to this entry when FileMetadata.Type == TypeReg.
	chunks []*internal.FileMetadata

	// skipSetAttrs is set when the file attributes must not be
	// modified, e.g. it is a hard link from a different source,
	// or a composefs file.
	skipSetAttrs bool
}

func doHardLink(srcFd int, destDirFd int, destBase string) error {
	doLink := func() error {
		// Using unix.AT_EMPTY_PATH requires CAP_DAC_READ_SEARCH while this variant that uses
		// /proc/self/fd doesn't and can be used with rootless.
		srcPath := procPathForFd(srcFd)
		return unix.Linkat(unix.AT_FDCWD, srcPath, destDirFd, destBase, unix.AT_SYMLINK_FOLLOW)
	}

	err := doLink()

	// if the destination exists, unlink it first and try again
	if err != nil && os.IsExist(err) {
		unix.Unlinkat(destDirFd, destBase, 0)
		return doLink()
	}
	return err
}

func copyFileContent(srcFd int, fileMetadata *fileMetadata, dirfd int, mode os.FileMode, useHardLinks bool) (*os.File, int64, error) {
	destFile := fileMetadata.Name
	src := procPathForFd(srcFd)
	st, err := os.Stat(src)
	if err != nil {
		return nil, -1, fmt.Errorf("copy file content for %q: %w", destFile, err)
	}

	copyWithFileRange, copyWithFileClone := true, true

	if useHardLinks {
		destDirPath, destBase := filepath.Split(destFile)
		destDir, err := openFileUnderRoot(dirfd, destDirPath, 0, 0)
		if err == nil {
			defer destDir.Close()

			err := doHardLink(srcFd, int(destDir.Fd()), destBase)
			if err == nil {
				// if the file was deduplicated with a hard link, skip overriding file metadata.
				fileMetadata.skipSetAttrs = true
				return nil, st.Size(), nil
			}
		}
	}

	// If the destination file already exists, we shouldn't blow it away
	dstFile, err := openFileUnderRoot(dirfd, destFile, newFileFlags, mode)
	if err != nil {
		return nil, -1, fmt.Errorf("open file %q under rootfs for copy: %w", destFile, err)
	}

	err = driversCopy.CopyRegularToFile(src, dstFile, st, &copyWithFileRange, &copyWithFileClone)
	if err != nil {
		dstFile.Close()
		return nil, -1, fmt.Errorf("copy to file %q under rootfs: %w", destFile, err)
	}
	return dstFile, st.Size(), nil
}

func timeToTimespec(time *time.Time) (ts unix.Timespec) {
	if time == nil || time.IsZero() {
		// Return UTIME_OMIT special value
		ts.Sec = 0
		ts.Nsec = ((1 << 30) - 2)
		return
	}
	return unix.NsecToTimespec(time.UnixNano())
}

// setFileAttrs sets the file attributes for file given metadata
func setFileAttrs(dirfd int, file *os.File, mode os.FileMode, metadata *fileMetadata, options *archive.TarOptions, usePath bool) error {
	if metadata.skipSetAttrs {
		return nil
	}
	if file == nil || file.Fd() < 0 {
		return errors.New("invalid file")
	}
	fd := int(file.Fd())

	t, err := typeToTarType(metadata.Type)
	if err != nil {
		return err
	}

	// If it is a symlink, force to use the path
	if t == tar.TypeSymlink {
		usePath = true
	}

	baseName := ""
	if usePath {
		dirName := filepath.Dir(metadata.Name)
		if dirName != "" {
			parentFd, err := openFileUnderRoot(dirfd, dirName, unix.O_PATH|unix.O_DIRECTORY, 0)
			if err != nil {
				return err
			}
			defer parentFd.Close()

			dirfd = int(parentFd.Fd())
		}
		baseName = filepath.Base(metadata.Name)
	}

	doChown := func() error {
		if usePath {
			return unix.Fchownat(dirfd, baseName, metadata.UID, metadata.GID, unix.AT_SYMLINK_NOFOLLOW)
		}
		return unix.Fchown(fd, metadata.UID, metadata.GID)
	}

	doSetXattr := func(k string, v []byte) error {
		return unix.Fsetxattr(fd, k, v, 0)
	}

	doUtimes := func() error {
		ts := []unix.Timespec{timeToTimespec(metadata.AccessTime), timeToTimespec(metadata.ModTime)}
		if usePath {
			return unix.UtimesNanoAt(dirfd, baseName, ts, unix.AT_SYMLINK_NOFOLLOW)
		}
		return unix.UtimesNanoAt(unix.AT_FDCWD, procPathForFd(fd), ts, 0)
	}

	doChmod := func() error {
		if usePath {
			return unix.Fchmodat(dirfd, baseName, uint32(mode), unix.AT_SYMLINK_NOFOLLOW)
		}
		return unix.Fchmod(fd, uint32(mode))
	}

	if err := doChown(); err != nil {
		if !options.IgnoreChownErrors {
			return fmt.Errorf("chown %q to %d:%d: %w", metadata.Name, metadata.UID, metadata.GID, err)
		}
	}

	canIgnore := func(err error) bool {
		return err == nil || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP)
	}

	for k, v := range metadata.Xattrs {
		if _, found := xattrsToIgnore[k]; found {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return fmt.Errorf("decode xattr %q: %w", v, err)
		}
		if err := doSetXattr(k, data); !canIgnore(err) {
			return fmt.Errorf("set xattr %s=%q for %q: %w", k, data, metadata.Name, err)
		}
	}

	if err := doUtimes(); !canIgnore(err) {
		return fmt.Errorf("set utimes for %q: %w", metadata.Name, err)
	}

	if err := doChmod(); !canIgnore(err) {
		return fmt.Errorf("chmod %q: %w", metadata.Name, err)
	}
	return nil
}

func openFileUnderRootFallback(dirfd int, name string, flags uint64, mode os.FileMode) (int, error) {
	root := procPathForFd(dirfd)

	targetRoot, err := os.Readlink(root)
	if err != nil {
		return -1, err
	}

	hasNoFollow := (flags & unix.O_NOFOLLOW) != 0

	var fd int
	// If O_NOFOLLOW is specified in the flags, then resolve only the parent directory and use the
	// last component as the path to openat().
	if hasNoFollow {
		dirName, baseName := filepath.Split(name)
		if dirName != "" && dirName != "." {
			newRoot, err := securejoin.SecureJoin(root, dirName)
			if err != nil {
				return -1, err
			}
			root = newRoot
		}

		parentDirfd, err := unix.Open(root, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return -1, err
		}
		defer unix.Close(parentDirfd)

		fd, err = unix.Openat(parentDirfd, baseName, int(flags), uint32(mode))
		if err != nil {
			return -1, err
		}
	} else {
		newPath, err := securejoin.SecureJoin(root, name)
		if err != nil {
			return -1, err
		}
		fd, err = unix.Openat(dirfd, newPath, int(flags), uint32(mode))
		if err != nil {
			return -1, err
		}
	}

	target, err := os.Readlink(procPathForFd(fd))
	if err != nil {
		unix.Close(fd)
		return -1, err
	}

	// Add an additional check to make sure the opened fd is inside the rootfs
	if !strings.HasPrefix(target, targetRoot) {
		unix.Close(fd)
		return -1, fmt.Errorf("while resolving %q.  It resolves outside the root directory", name)
	}

	return fd, err
}

func openFileUnderRootOpenat2(dirfd int, name string, flags uint64, mode os.FileMode) (int, error) {
	how := unix.OpenHow{
		Flags:   flags,
		Mode:    uint64(mode & 0o7777),
		Resolve: unix.RESOLVE_IN_ROOT,
	}
	return unix.Openat2(dirfd, name, &how)
}

// skipOpenat2 is set when openat2 is not supported by the underlying kernel and avoid
// using it again.
var skipOpenat2 int32

// openFileUnderRootRaw tries to open a file using openat2 and if it is not supported fallbacks to a
// userspace lookup.
func openFileUnderRootRaw(dirfd int, name string, flags uint64, mode os.FileMode) (int, error) {
	var fd int
	var err error
	if name == "" {
		return unix.Dup(dirfd)
	}
	if atomic.LoadInt32(&skipOpenat2) > 0 {
		fd, err = openFileUnderRootFallback(dirfd, name, flags, mode)
	} else {
		fd, err = openFileUnderRootOpenat2(dirfd, name, flags, mode)
		// If the function failed with ENOSYS, switch off the support for openat2
		// and fallback to using safejoin.
		if err != nil && errors.Is(err, unix.ENOSYS) {
			atomic.StoreInt32(&skipOpenat2, 1)
			fd, err = openFileUnderRootFallback(dirfd, name, flags, mode)
		}
	}
	return fd, err
}

// openFileUnderRoot safely opens a file under the specified root directory using openat2
// dirfd is an open file descriptor to the target checkout directory.
// name is the path to open relative to dirfd.
// flags are the flags to pass to the open syscall.
// mode specifies the mode to use for newly created files.
func openFileUnderRoot(dirfd int, name string, flags uint64, mode os.FileMode) (*os.File, error) {
	fd, err := openFileUnderRootRaw(dirfd, name, flags, mode)
	if err == nil {
		return os.NewFile(uintptr(fd), name), nil
	}

	hasCreate := (flags & unix.O_CREAT) != 0
	if errors.Is(err, unix.ENOENT) && hasCreate {
		parent := filepath.Dir(name)
		if parent != "" {
			newDirfd, err2 := openOrCreateDirUnderRoot(dirfd, parent, 0)
			if err2 == nil {
				defer newDirfd.Close()
				fd, err := openFileUnderRootRaw(int(newDirfd.Fd()), filepath.Base(name), flags, mode)
				if err == nil {
					return os.NewFile(uintptr(fd), name), nil
				}
			}
		}
	}
	return nil, fmt.Errorf("open %q under the rootfs: %w", name, err)
}

// openOrCreateDirUnderRoot safely opens a directory or create it if it is missing.
// dirfd is an open file descriptor to the target checkout directory.
// name is the path to open relative to dirfd.
// mode specifies the mode to use for newly created files.
func openOrCreateDirUnderRoot(dirfd int, name string, mode os.FileMode) (*os.File, error) {
	fd, err := openFileUnderRootRaw(dirfd, name, unix.O_DIRECTORY|unix.O_RDONLY, 0)
	if err == nil {
		return os.NewFile(uintptr(fd), name), nil
	}

	if errors.Is(err, unix.ENOENT) {
		parent := filepath.Dir(name)
		if parent != "" {
			pDir, err2 := openOrCreateDirUnderRoot(dirfd, parent, mode)
			if err2 != nil {
				return nil, err
			}
			defer pDir.Close()

			baseName := filepath.Base(name)

			if err2 := unix.Mkdirat(int(pDir.Fd()), baseName, uint32(mode)); err2 != nil {
				return nil, err
			}

			fd, err = openFileUnderRootRaw(int(pDir.Fd()), baseName, unix.O_DIRECTORY|unix.O_RDONLY, 0)
			if err == nil {
				return os.NewFile(uintptr(fd), name), nil
			}
		}
	}
	return nil, err
}

// appendHole creates a hole with the specified size at the open fd.
func appendHole(fd int, size int64) error {
	off, err := unix.Seek(fd, size, unix.SEEK_CUR)
	if err != nil {
		return err
	}
	// Make sure the file size is changed.  It might be the last hole and no other data written afterwards.
	if err := unix.Ftruncate(fd, off); err != nil {
		return err
	}
	return nil
}

func safeMkdir(dirfd int, mode os.FileMode, name string, metadata *fileMetadata, options *archive.TarOptions) error {
	parent, base := filepath.Split(name)
	parentFd := dirfd
	if parent != "" && parent != "." {
		parentFile, err := openOrCreateDirUnderRoot(dirfd, parent, 0)
		if err != nil {
			return err
		}
		defer parentFile.Close()
		parentFd = int(parentFile.Fd())
	}

	if err := unix.Mkdirat(parentFd, base, uint32(mode)); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("mkdir %q: %w", name, err)
		}
	}

	file, err := openFileUnderRoot(parentFd, base, unix.O_DIRECTORY|unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	return setFileAttrs(dirfd, file, mode, metadata, options, false)
}

func safeLink(dirfd int, mode os.FileMode, metadata *fileMetadata, options *archive.TarOptions) error {
	sourceFile, err := openFileUnderRoot(dirfd, metadata.Linkname, unix.O_PATH|unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destDir, destBase := filepath.Split(metadata.Name)
	destDirFd := dirfd
	if destDir != "" && destDir != "." {
		f, err := openOrCreateDirUnderRoot(dirfd, destDir, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		destDirFd = int(f.Fd())
	}

	err = doHardLink(int(sourceFile.Fd()), destDirFd, destBase)
	if err != nil {
		return fmt.Errorf("create hardlink %q pointing to %q: %w", metadata.Name, metadata.Linkname, err)
	}

	newFile, err := openFileUnderRoot(dirfd, metadata.Name, unix.O_WRONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		// If the target is a symlink, open the file with O_PATH.
		if errors.Is(err, unix.ELOOP) {
			newFile, err := openFileUnderRoot(dirfd, metadata.Name, unix.O_PATH|unix.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			defer newFile.Close()

			return setFileAttrs(dirfd, newFile, mode, metadata, options, true)
		}
		return err
	}
	defer newFile.Close()

	return setFileAttrs(dirfd, newFile, mode, metadata, options, false)
}

func safeSymlink(dirfd int, mode os.FileMode, metadata *fileMetadata, options *archive.TarOptions) error {
	destDir, destBase := filepath.Split(metadata.Name)
	destDirFd := dirfd
	if destDir != "" && destDir != "." {
		f, err := openOrCreateDirUnderRoot(dirfd, destDir, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		destDirFd = int(f.Fd())
	}

	if err := unix.Symlinkat(metadata.Linkname, destDirFd, destBase); err != nil {
		return fmt.Errorf("create symlink %q pointing to %q: %w", metadata.Name, metadata.Linkname, err)
	}
	return nil
}

type whiteoutHandler struct {
	Dirfd int
	Root  string
}

func (d whiteoutHandler) Setxattr(path, name string, value []byte) error {
	file, err := openOrCreateDirUnderRoot(d.Dirfd, path, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	if err := unix.Fsetxattr(int(file.Fd()), name, value, 0); err != nil {
		return fmt.Errorf("set xattr %s=%q for %q: %w", name, value, path, err)
	}
	return nil
}

func (d whiteoutHandler) Mknod(path string, mode uint32, dev int) error {
	dir, base := filepath.Split(path)
	dirfd := d.Dirfd
	if dir != "" && dir != "." {
		dir, err := openOrCreateDirUnderRoot(d.Dirfd, dir, 0)
		if err != nil {
			return err
		}
		defer dir.Close()

		dirfd = int(dir.Fd())
	}

	if err := unix.Mknodat(dirfd, base, mode, dev); err != nil {
		return fmt.Errorf("mknod %q: %w", path, err)
	}

	return nil
}

func checkChownErr(err error, name string, uid, gid int) error {
	if errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf(`potentially insufficient UIDs or GIDs available in user namespace (requested %d:%d for %s): Check /etc/subuid and /etc/subgid if configured locally and run "podman system migrate": %w`, uid, gid, name, err)
	}
	return err
}

func (d whiteoutHandler) Chown(path string, uid, gid int) error {
	file, err := openFileUnderRoot(d.Dirfd, path, unix.O_PATH, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	if err := unix.Fchownat(int(file.Fd()), "", uid, gid, unix.AT_EMPTY_PATH); err != nil {
		var stat unix.Stat_t
		if unix.Fstat(int(file.Fd()), &stat) == nil {
			if stat.Uid == uint32(uid) && stat.Gid == uint32(gid) {
				return nil
			}
		}
		return checkChownErr(err, path, uid, gid)
	}
	return nil
}

type readerAtCloser interface {
	io.ReaderAt
	io.Closer
}

// seekableFile is a struct that wraps an *os.File to provide an ImageSourceSeekable.
type seekableFile struct {
	reader readerAtCloser
}

func (f *seekableFile) Close() error {
	return f.reader.Close()
}

func (f *seekableFile) GetBlobAt(chunks []ImageSourceChunk) (chan io.ReadCloser, chan error, error) {
	streams := make(chan io.ReadCloser)
	errs := make(chan error)

	go func() {
		for _, chunk := range chunks {
			streams <- io.NopCloser(io.NewSectionReader(f.reader, int64(chunk.Offset), int64(chunk.Length)))
		}
		close(streams)
		close(errs)
	}()

	return streams, errs, nil
}

func newSeekableFile(reader readerAtCloser) *seekableFile {
	return &seekableFile{reader: reader}
}