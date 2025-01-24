// Package vfsstore provide a storage backend based on the local file system.
//
// VFSStore is a storage backend used as a handler.DataStore in handler.NewHandler.
// It stores the uploads in a directory specified in two different files: The
// `[id].info` files are used to store the fileinfo in JSON format. The
// `[id]` files without an extension contain the raw binary data uploaded.
// No cleanup is performed so you may want to run a cronjob to ensure your disk
// is not filled up with old and finished uploads.
//
// Related to the VFSStore is the package filelocker, which provides a file-based
// locking mechanism. The use of some locking method is recommended and further
// explained in https://tus.github.io/tusd/advanced-topics/locks/.
package vfsstore

import (
	"context"
	"emperror.dev/errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/je4/filesystem/v3/pkg/writefs"
	"github.com/je4/utils/v2/pkg/zLogger"
	"github.com/tus/tusd/v2/pkg/handler"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

func VFSPathJoin(elem ...string) string {
	if len(elem) == 0 {
		return ""
	}
	if !strings.HasPrefix(elem[0], "vfs://") {
		return filepath.ToSlash(filepath.Join(elem...))
	}
	return "vfs://" + filepath.ToSlash(filepath.Join(append([]string{elem[0][6:]}, elem[1:]...)...))
}

func VFSPathClean(p string) string {
	if !strings.HasPrefix(p, "vfs://") {
		return filepath.ToSlash(filepath.Clean(p))
	}
	return "vfs://" + filepath.ToSlash(filepath.Clean(p[6:]))
}

var pathAllowedChars = regexp.MustCompile(`^[a-zA-Z0-9_\-/.+ ]+$`)

func VFSPathCheck(p string) bool {
	if strings.HasPrefix(p, "vfs://") {
		p = p[6:]
	}

	return false
}

// See the handler.DataStore interface for documentation about the different
// methods.
type VFSStore struct {
	// Relative or absolute path to store files in. VFSStore does not check
	// whether the path exists, use os.MkdirAll in this case on your own.
	defaultPath string

	// List of path prefixes that are allowed to be used as upload folders
	allowedPaths []string

	// virtual filesystem implements writefs
	vfs fs.FS

	uploads       map[string]*fileUpload
	uploadsLock   *sync.RWMutex
	maxUploadTime time.Duration
	logger        zLogger.ZLogger
}

// New creates a new file based storage backend. The directory specified will
// be used as the only storage entry. This method does not check
// whether the path exists, use os.MkdirAll to ensure.
func New(vfs fs.FS, defaultPath string, allowedPaths []string, duration time.Duration, logger zLogger.ZLogger) VFSStore {
	return VFSStore{
		defaultPath:   defaultPath,
		allowedPaths:  allowedPaths,
		vfs:           vfs,
		uploads:       make(map[string]*fileUpload),
		uploadsLock:   &sync.RWMutex{},
		maxUploadTime: duration,
		logger:        logger,
	}
}

// UseIn sets this store as the core data store in the passed composer and adds
// all possible extension to it.
func (store VFSStore) UseIn(composer *handler.StoreComposer) {
	composer.UseCore(store)
	//composer.UseTerminater(store)
	//composer.UseConcater(store)
	composer.UseLengthDeferrer(store)
	//composer.UseContentServer(store)
	//composer.UseLocker(store)
}

func (store VFSStore) NewUpload(ctx context.Context, info handler.FileInfo) (handler.Upload, error) {
	var basePath string
	if basePath = info.MetaData["basePath"]; basePath == "" {
		basePath = store.defaultPath
	}
	if info.ID == "" {
		info.ID = uuid.New().String()
	}

	if _, ok := store.getUpload(info.ID); ok {
		return nil, errors.New("upload with the same ID already exists")
	}

	// The binary file's location might be modified by the pre-create hook.
	var binPath string
	if info.Storage != nil && info.Storage["Path"] != "" {
		// filepath.Join treats absolute and relative paths the same, so we must
		// handle them on our own. Absolute paths get used as-is, while relative
		// paths are joined to the storage path.
		if strings.HasPrefix(info.Storage["Path"], "vfs://") {
			binPath = info.Storage["Path"]
		} else {
			binPath = VFSPathJoin(basePath, info.Storage["Path"])
		}
	} else {
		binPath = VFSPathJoin(basePath, info.ID)
	}

	// check if path is allowed
	pathAllowed := false
	for _, allowedPath := range store.allowedPaths {
		if strings.HasPrefix(binPath, allowedPath) {
			pathAllowed = true
			break
		}
	}
	if !pathAllowed {
		return nil, fmt.Errorf("path not allowed: %s", binPath)
	}

	info.Storage = map[string]string{
		"Type":        "VFSStore",
		"defaultPath": binPath,
	}

	// Create binary file with no content
	fp, err := writefs.Create(store.vfs, binPath)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot create file %s", binPath)
	}

	upload := newUpload(info, binPath, fp, &store)

	return upload, nil
}

func (store VFSStore) GetUpload(ctx context.Context, id string) (handler.Upload, error) {
	id = strings.TrimPrefix(id, "files/")
	upload, ok := store.getUpload(id)
	if !ok {
		return nil, errors.Wrapf(handler.ErrNotFound, "upload with id %s not found", id)
	}
	return upload, nil
}

func (store VFSStore) AsLengthDeclarableUpload(upload handler.Upload) handler.LengthDeclarableUpload {
	return upload.(*fileUpload)
}

func (store VFSStore) addUpload(upload *fileUpload) {
	store.uploadsLock.Lock()
	defer store.uploadsLock.Unlock()
	store.uploads[upload.info.ID] = upload
}

func (store VFSStore) removeUpload(id string) {
	store.uploadsLock.Lock()
	defer store.uploadsLock.Unlock()
	delete(store.uploads, id)
}

func (store VFSStore) getUpload(id string) (*fileUpload, bool) {
	store.uploadsLock.RLock()
	defer store.uploadsLock.RUnlock()
	upload, ok := store.uploads[id]
	return upload, ok
}
