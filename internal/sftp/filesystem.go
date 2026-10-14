package sftp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	pkgsftp "github.com/pkg/sftp"

	"hivepanel-worker/internal/panel"
)

var (
	errPermissionDenied = errors.New("permission denied")
	errLinksUnsupported = errors.New("links are not supported")
)

type JailHandlerFactory struct{}

type jailedFilesystem struct {
	root        string
	permissions panel.SFTPPermissions
}

type fileInfoLister struct {
	entries []os.FileInfo
}

func NewJailHandlerFactory() *JailHandlerFactory {
	return &JailHandlerFactory{}
}

func (factory *JailHandlerFactory) Create(
	root string,
	permissions panel.SFTPPermissions,
) (pkgsftp.Handlers, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return pkgsftp.Handlers{}, fmt.Errorf(
			"resolve SFTP root: %w",
			err,
		)
	}

	rootInfo, err := os.Stat(absoluteRoot)
	if err != nil {
		return pkgsftp.Handlers{}, fmt.Errorf(
			"inspect SFTP root: %w",
			err,
		)
	}

	if !rootInfo.IsDir() {
		return pkgsftp.Handlers{}, fmt.Errorf(
			"SFTP root is not a directory",
		)
	}

	filesystem := &jailedFilesystem{
		root:        filepath.Clean(absoluteRoot),
		permissions: permissions,
	}

	return pkgsftp.Handlers{
		FileGet:  filesystem,
		FilePut:  filesystem,
		FileCmd:  filesystem,
		FileList: filesystem,
	}, nil
}

/*
	File reading
*/

func (filesystem *jailedFilesystem) Fileread(
	request *pkgsftp.Request,
) (io.ReaderAt, error) {
	if !filesystem.permissions.Read {
		return nil, errPermissionDenied
	}

	resolved, err := filesystem.resolveExisting(
		request.Filepath,
		false,
	)
	if err != nil {
		return nil, err
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return nil, err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errLinksUnsupported
	}

	if info.IsDir() {
		return nil, fmt.Errorf("cannot read a directory")
	}

	return os.Open(resolved)
}

/*
	File writing
*/

func (filesystem *jailedFilesystem) Filewrite(
	request *pkgsftp.Request,
) (io.WriterAt, error) {
	if err := filesystem.ensureWritable(); err != nil {
		return nil, err
	}
	flags := request.Pflags()

	if !filesystem.permissions.Write {
		return nil, errPermissionDenied
	}

	if flags.Creat && !filesystem.permissions.Create {
		return nil, errPermissionDenied
	}

	resolved, err := filesystem.resolveForWrite(
		request.Filepath,
	)
	if err != nil {
		return nil, err
	}

	openFlags := 0

	if flags.Read && flags.Write {
		openFlags |= os.O_RDWR
	} else {
		openFlags |= os.O_WRONLY
	}

	if flags.Creat {
		openFlags |= os.O_CREATE
	}

	if flags.Trunc {
		openFlags |= os.O_TRUNC
	}

	if flags.Excl {
		openFlags |= os.O_EXCL
	}

	/*
		Do not use O_APPEND here. pkg/sftp performs offset-based WriteAt
		operations and O_APPEND conflicts with WriteAt semantics.
	*/
	file, err := os.OpenFile(
		resolved,
		openFlags,
		0644,
	)
	if err != nil {
		return nil, err
	}

	return &lockedWriter{File: file, filesystem: filesystem}, nil
}

/*
	Filesystem commands
*/

func (filesystem *jailedFilesystem) Filecmd(
	request *pkgsftp.Request,
) error {
	if err := filesystem.ensureWritable(); err != nil {
		return err
	}
	switch request.Method {
	case "Mkdir":
		return filesystem.mkdir(request)

	case "Rmdir":
		return filesystem.rmdir(request)

	case "Remove":
		return filesystem.remove(request)

	case "Rename", "PosixRename":
		return filesystem.rename(request)

	case "Setstat":
		return filesystem.setstat(request)

	case "Symlink", "Link":
		return errLinksUnsupported

	default:
		return fmt.Errorf(
			"unsupported SFTP command: %s",
			request.Method,
		)
	}
}

func (filesystem *jailedFilesystem) mkdir(
	request *pkgsftp.Request,
) error {
	if !filesystem.permissions.Create {
		return errPermissionDenied
	}

	resolved, err := filesystem.resolveForWrite(
		request.Filepath,
	)
	if err != nil {
		return err
	}

	mode := os.FileMode(0755)

	if attributes := request.Attributes(); attributes != nil {
		if request.AttrFlags().Permissions {
			mode = attributes.FileMode().Perm()
		}
	}

	return os.Mkdir(resolved, mode)
}

func (filesystem *jailedFilesystem) rmdir(
	request *pkgsftp.Request,
) error {
	if !filesystem.permissions.Delete {
		return errPermissionDenied
	}

	resolved, err := filesystem.resolveExisting(
		request.Filepath,
		false,
	)
	if err != nil {
		return err
	}

	if filesystem.isRoot(resolved) {
		return errPermissionDenied
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return errLinksUnsupported
	}

	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}

	return os.Remove(resolved)
}

func (filesystem *jailedFilesystem) remove(
	request *pkgsftp.Request,
) error {
	if !filesystem.permissions.Delete {
		return errPermissionDenied
	}

	resolved, err := filesystem.resolveExisting(
		request.Filepath,
		false,
	)
	if err != nil {
		return err
	}

	if filesystem.isRoot(resolved) {
		return errPermissionDenied
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return errLinksUnsupported
	}

	if info.IsDir() {
		return fmt.Errorf("path is a directory")
	}

	return os.Remove(resolved)
}

func (filesystem *jailedFilesystem) rename(
	request *pkgsftp.Request,
) error {
	if !filesystem.permissions.Rename {
		return errPermissionDenied
	}

	source, err := filesystem.resolveExisting(
		request.Filepath,
		false,
	)
	if err != nil {
		return err
	}

	target, err := filesystem.resolveForWrite(
		request.Target,
	)
	if err != nil {
		return err
	}

	if filesystem.isRoot(source) || filesystem.isRoot(target) {
		return errPermissionDenied
	}

	sourceInfo, err := os.Lstat(source)
	if err != nil {
		return err
	}

	if sourceInfo.Mode()&os.ModeSymlink != 0 {
		return errLinksUnsupported
	}

	/*
		If the target already exists, ensure it is not a symlink before
		allowing an overwrite.
	*/
	if targetInfo, targetErr := os.Lstat(target); targetErr == nil {
		if targetInfo.Mode()&os.ModeSymlink != 0 {
			return errLinksUnsupported
		}
	} else if !os.IsNotExist(targetErr) {
		return targetErr
	}

	return os.Rename(source, target)
}

func (filesystem *jailedFilesystem) setstat(
	request *pkgsftp.Request,
) error {
	if !filesystem.permissions.Write {
		return errPermissionDenied
	}

	resolved, err := filesystem.resolveExisting(
		request.Filepath,
		false,
	)
	if err != nil {
		return err
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return errLinksUnsupported
	}

	attributes := request.Attributes()
	flags := request.AttrFlags()

	if attributes == nil {
		return nil
	}

	if flags.Size {
		if info.IsDir() {
			return fmt.Errorf("cannot truncate a directory")
		}

		if err := os.Truncate(
			resolved,
			int64(attributes.Size),
		); err != nil {
			return err
		}
	}

	if flags.Permissions {
		if err := os.Chmod(
			resolved,
			attributes.FileMode().Perm(),
		); err != nil {
			return err
		}
	}

	if flags.Acmodtime {
		accessTime := attributes.AccessTime()
		modificationTime := attributes.ModTime()

		if accessTime.IsZero() {
			accessTime = time.Now()
		}

		if modificationTime.IsZero() {
			modificationTime = time.Now()
		}

		if err := os.Chtimes(
			resolved,
			accessTime,
			modificationTime,
		); err != nil {
			return err
		}
	}

	/*
		UID/GID changes are intentionally ignored. The worker should retain
		ownership of all files inside its cell directories.
	*/

	return nil
}

/*
	Directory listing and stat operations
*/

func (filesystem *jailedFilesystem) Filelist(
	request *pkgsftp.Request,
) (pkgsftp.ListerAt, error) {
	if !filesystem.permissions.Read {
		return nil, errPermissionDenied
	}

	switch request.Method {
	case "List":
		return filesystem.listDirectory(request.Filepath)

	case "Stat":
		return filesystem.statPath(request.Filepath)

	case "Readlink":
		return nil, errLinksUnsupported

	default:
		return nil, fmt.Errorf(
			"unsupported SFTP list operation: %s",
			request.Method,
		)
	}
}

func (filesystem *jailedFilesystem) listDirectory(
	virtualPath string,
) (pkgsftp.ListerAt, error) {
	resolved, err := filesystem.resolveExisting(
		virtualPath,
		false,
	)
	if err != nil {
		return nil, err
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return nil, err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errLinksUnsupported
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory")
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil, err
	}

	fileInfos := make([]os.FileInfo, 0, len(entries))

	for _, entry := range entries {
		entryInfo, infoErr := entry.Info()
		if infoErr != nil {
			return nil, infoErr
		}

		/*
			Hide symlinks completely. This avoids presenting entries which
			cannot safely be followed within the jail.
		*/
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}

		fileInfos = append(fileInfos, entryInfo)
	}

	return &fileInfoLister{
		entries: fileInfos,
	}, nil
}

func (filesystem *jailedFilesystem) statPath(
	virtualPath string,
) (pkgsftp.ListerAt, error) {
	resolved, err := filesystem.resolveExisting(
		virtualPath,
		false,
	)
	if err != nil {
		return nil, err
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return nil, err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errLinksUnsupported
	}

	return &fileInfoLister{
		entries: []os.FileInfo{info},
	}, nil
}

func (lister *fileInfoLister) ListAt(
	destination []os.FileInfo,
	offset int64,
) (int, error) {
	if offset < 0 {
		return 0, fmt.Errorf("invalid list offset")
	}

	if offset >= int64(len(lister.entries)) {
		return 0, io.EOF
	}

	count := copy(
		destination,
		lister.entries[offset:],
	)

	if int(offset)+count >= len(lister.entries) {
		return count, io.EOF
	}

	return count, nil
}

/*
	Path jail
*/

func (filesystem *jailedFilesystem) resolveExisting(
	virtualPath string,
	allowRoot bool,
) (string, error) {
	resolved, err := filesystem.resolveVirtualPath(
		virtualPath,
	)
	if err != nil {
		return "", err
	}

	if !allowRoot && filesystem.isRoot(resolved) {
		/*
			The root may still be listed and statted. This protection is
			primarily for callers that mutate paths.
		*/
	}

	if err := filesystem.rejectSymlinkComponents(
		resolved,
		true,
	); err != nil {
		return "", err
	}

	return resolved, nil
}

func (filesystem *jailedFilesystem) resolveForWrite(
	virtualPath string,
) (string, error) {
	resolved, err := filesystem.resolveVirtualPath(
		virtualPath,
	)
	if err != nil {
		return "", err
	}

	if filesystem.isRoot(resolved) {
		return "", errPermissionDenied
	}

	/*
		The destination may not exist yet, so validate all existing parent
		components and then separately inspect the destination if present.
	*/
	if err := filesystem.rejectSymlinkComponents(
		filepath.Dir(resolved),
		true,
	); err != nil {
		return "", err
	}

	if info, statErr := os.Lstat(resolved); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errLinksUnsupported
		}
	} else if !os.IsNotExist(statErr) {
		return "", statErr
	}

	return resolved, nil
}

func (filesystem *jailedFilesystem) resolveVirtualPath(
	virtualPath string,
) (string, error) {
	virtualPath = strings.TrimSpace(virtualPath)

	if virtualPath == "" {
		virtualPath = "/"
	}

	/*
		SFTP paths use POSIX separators regardless of the host OS.
		Cleaning with path.Clean prevents ordinary "../" traversal.
	*/
	cleaned := path.Clean("/" + strings.TrimPrefix(
		filepath.ToSlash(virtualPath),
		"/",
	))

	relative := strings.TrimPrefix(cleaned, "/")

	resolved := filepath.Clean(
		filepath.Join(
			filesystem.root,
			filepath.FromSlash(relative),
		),
	)

	if err := filesystem.ensureInsideRoot(resolved); err != nil {
		return "", err
	}

	return resolved, nil
}

func (filesystem *jailedFilesystem) ensureInsideRoot(
	resolved string,
) error {
	relative, err := filepath.Rel(
		filesystem.root,
		resolved,
	)
	if err != nil {
		return fmt.Errorf(
			"validate SFTP path: %w",
			err,
		)
	}

	if relative == ".." ||
		strings.HasPrefix(
			relative,
			".."+string(os.PathSeparator),
		) {
		return errPermissionDenied
	}

	return nil
}

func (filesystem *jailedFilesystem) rejectSymlinkComponents(
	resolved string,
	includeFinal bool,
) error {
	if err := filesystem.ensureInsideRoot(resolved); err != nil {
		return err
	}

	relative, err := filepath.Rel(
		filesystem.root,
		resolved,
	)
	if err != nil {
		return err
	}

	if relative == "." {
		return nil
	}

	components := strings.Split(
		relative,
		string(os.PathSeparator),
	)

	current := filesystem.root

	for index, component := range components {
		if component == "" || component == "." {
			continue
		}

		current = filepath.Join(current, component)

		if !includeFinal && index == len(components)-1 {
			break
		}

		info, statErr := os.Lstat(current)
		if statErr != nil {
			return statErr
		}

		if info.Mode()&os.ModeSymlink != 0 {
			return errLinksUnsupported
		}
	}

	return nil
}

func (filesystem *jailedFilesystem) isRoot(
	resolved string,
) bool {
	return filepath.Clean(resolved) == filesystem.root
}

// Long-lived SFTP sessions honor locks created after authentication, including
// handles opened before an installer reserves the Cell.
func (f *jailedFilesystem) ensureWritable() error {
	data, err := os.ReadFile(filepath.Join(f.root, ".hivepanel", "lock.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var lock struct {
		Locked       bool `json:"locked"`
		DisableFiles bool `json:"disable_files"`
	}
	if json.Unmarshal(data, &lock) != nil {
		return errPermissionDenied
	}
	if lock.Locked && lock.DisableFiles {
		return errPermissionDenied
	}
	return nil
}

type lockedWriter struct {
	*os.File
	filesystem *jailedFilesystem
}

func (w *lockedWriter) WriteAt(p []byte, off int64) (int, error) {
	if err := w.filesystem.ensureWritable(); err != nil {
		return 0, err
	}
	return w.File.WriteAt(p, off)
}
