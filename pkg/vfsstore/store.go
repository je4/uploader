// Package vfsstore provide a storage backend based on the local file system.
//
// VFSStore is a storage backend used as a handler.DataStore in handler.NewHandler.
// It stores the uploads in a directory specified in two different files: The
// `[id].info` files are used to store the fileinfo in JSON format. The
// `[id]` files without an extension contain the raw binary data uploaded.
// No cleanup is performed so you may want to run a cronjob to ensure your disk
// is not filled up with old and finished uploads.
//
// Related to the filestore is the package filelocker, which provides a file-based
// locking mechanism. The use of some locking method is recommended and further
// explained in https://tus.github.io/tusd/advanced-topics/locks/.
package vfsstore

import (
	"context"
	"emperror.dev/errors"
	"fmt"
	"github.com/bluele/gcache"
	"github.com/google/uuid"
	"github.com/je4/filesystem/v3/pkg/writefs"
	"github.com/je4/utils/v2/pkg/zLogger"
	"github.com/tus/tusd/v2/pkg/handler"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// VFSStore See the handler.DataStore interface for documentation about the different
// methods.
type VFSStore struct {
	fsys        fs.FS
	uploadPath  string
	cachePath   string
	filePath    string
	uploadCache gcache.Cache
}

// New creates a new file based storage backend. The directory specified will
// be used as the only storage entry. This method does not check
// whether the path exists, use os.MkdirAll to ensure.
func New(fsys fs.FS, uploadPath, cachePath, filePath string, cacheExpiration time.Duration, logger zLogger.ZLogger) VFSStore {
	vfsStore := &VFSStore{
		fsys:       fsys,
		uploadPath: strings.TrimRight(uploadPath, "/"),
		cachePath:  strings.TrimRight(cachePath, "/"),
		filePath:   strings.Trim(filepath.ToSlash(filepath.Clean(filePath)), "/"),
		uploadCache: gcache.New(100).LRU().Expiration(cacheExpiration).EvictedFunc(func(key any, value any) {
			upload, ok := value.(*vfsUpload)
			if !ok {
				logger.Error().Msgf("cannot cast value of key %v to *vfsUpload", key)
				return
			}
			if err := upload.Terminate(context.Background()); err != nil {
				logger.Error().Err(err).Msgf("cannot terminate upload %v", key)
				return
			}
		}).Build(),
	}
	return *vfsStore
}

// UseIn sets this store as the core data store in the passed composer and adds
// all possible extension to it.
func (store VFSStore) UseIn(composer *handler.StoreComposer) {
	composer.UseCore(store)
	composer.UseTerminater(store)
	composer.UseConcater(store)
	composer.UseLengthDeferrer(store)
}

func (store VFSStore) NewUpload(ctx context.Context, info handler.FileInfo) (handler.Upload, error) {
	if info.ID == "" {
		info.ID = uuid.New().String()
	}

	// The binary file's location might be modified by the pre-create hook.
	var binPath string
	if info.Storage != nil && info.Storage["Path"] != "" {
		// filepath.Join treats absolute and relative paths the same, so we must
		// handle them on our own. Absolute paths get used as-is, while relative
		// paths are joined to the storage path.
		if strings.HasPrefix(info.Storage["Path"], "vfs:") {
			binPath = info.Storage["Path"]
		} else {
			binPath = store.uploadPath + "/" + filepath.ToSlash(filepath.Clean(info.Storage["Path"]))
		}
	} else {
		binPath = store.defaultBinPath(info.ID)
	}

	info.Storage = map[string]string{
		"Type": "vfsstore",
		"Path": binPath,
	}

	// Create binary file with no content
	file, err := writefs.Create(store.fsys, binPath)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot create file '%s'", binPath)
	}

	upload := &vfsUpload{
		fsys:    store.fsys,
		file:    file,
		info:    info,
		binPath: binPath,
	}

	store.uploadCache.Set(info.ID, upload)

	/*
		// writeInfo creates the file by itself if necessary
		if err := upload.writeInfo(); err != nil {
			return nil, err
		}
	*/

	return upload, nil
}

func (store VFSStore) GetUpload(ctx context.Context, id string) (handler.Upload, error) {
	id = strings.TrimPrefix(id, store.filePath+"/")

	uploadAny, err := store.uploadCache.Get(id)
	if err != nil {
		if !errors.Is(err, gcache.KeyNotFoundError) {
			return nil, handler.ErrNotFound
		}
		return nil, errors.Wrapf(err, "cannot get upload %v from cache", id)
	}
	upload, ok := uploadAny.(*vfsUpload)
	if !ok {
		return nil, errors.Errorf("cannot cast value of key %v to *vfsUpload", id)
	}
	if upload.file == nil {
		store.uploadCache.Remove(id)
		return nil, handler.ErrNotFound
	}
	return upload, nil
}

func (store VFSStore) AsTerminatableUpload(upload handler.Upload) handler.TerminatableUpload {
	return upload.(*vfsUpload)
}

func (store VFSStore) AsLengthDeclarableUpload(upload handler.Upload) handler.LengthDeclarableUpload {
	return upload.(*vfsUpload)
}

func (store VFSStore) AsConcatableUpload(upload handler.Upload) handler.ConcatableUpload {
	return upload.(*vfsUpload)
}

// defaultBinPath returns the path to the file storing the binary data, if it is
// not customized using the pre-create hook.
func (store VFSStore) defaultBinPath(id string) string {
	return store.uploadPath + "/" + filepath.ToSlash(filepath.Clean(filepath.Join(store.filePath, id)))
}

// infoPath returns the path to the .info file storing the file's info.
func (store VFSStore) infoPath(id string) string {
	return store.cachePath + "/" + filepath.ToSlash(filepath.Clean(filepath.Join(store.filePath, id+".info")))
}

// createFile creates the file with the content. If the corresponding directory does not exist,
// it is created. If the file already exists, its content is removed.
func createFile(fsys fs.FS, path string, content []byte) error {
	fileinfo, err := fs.Stat(fsys, path)
	if err == nil {
		if fileinfo.IsDir() {
			return fmt.Errorf("cannot create file '%s': path is a directory", path)
		}
		if err := writefs.Remove(fsys, path); err != nil {
			return fmt.Errorf("cannot remove existing file '%s': %s", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cannot stat '%s': %s", path, err)
	}
	if content == nil {
		content = []byte{}
	}
	if _, err := writefs.WriteFile(fsys, path, content); err != nil {
		return errors.Wrapf(err, "cannot create and write file '%s'", path)
	}
	return nil
}
