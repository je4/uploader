package vfsstore

import (
	"context"
	"emperror.dev/errors"
	"github.com/je4/filesystem/v3/pkg/writefs"
	"github.com/je4/utils/v2/pkg/zLogger"
	"github.com/tus/tusd/v2/pkg/handler"
	"io"
	"io/fs"
	"sync"
	"sync/atomic"
	"time"
)

func NewUpload(binPath string, store *VFSStore, info handler.FileInfo, logger zLogger.ZLogger) (*vfsUpload, error) {
	file, err := writefs.Create(store.fsys, binPath)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot create file '%s'", binPath)
	}
	upload := &vfsUpload{
		file:       file,
		store:      store,
		info:       info,
		binPath:    binPath,
		lastAccess: atomicTime{},
		logger:     logger,
		stop:       make(chan struct{}),
		stopMutex:  sync.Mutex{},
	}
	upload.lastAccess.Store(time.Now())
	go upload.Run()
	return upload, nil
}

type atomicTime atomic.Value

func (t *atomicTime) Load() time.Time {
	return (*atomic.Value)(t).Load().(time.Time)
}

func (t *atomicTime) Store(v time.Time) {
	(*atomic.Value)(t).Store(v)
}

func (t *atomicTime) Swap(v time.Time) time.Time {
	return (*atomic.Value)(t).Swap(v).(time.Time)
}

func (t *atomicTime) CompareAndSwap(old, new time.Time) bool {
	return (*atomic.Value)(t).CompareAndSwap(old, new)
}

type vfsUpload struct {
	store *VFSStore
	file  io.WriteCloser

	// info stores the current information about the upload
	info handler.FileInfo
	// binPath is the path to the binary file (which has no extension)
	binPath    string
	lastAccess atomicTime
	stop       chan struct{}
	isRunning  atomic.Bool
	logger     zLogger.ZLogger
	stopMutex  sync.Mutex
}

func (upload *vfsUpload) Stop() error {
	upload.stopMutex.Lock()
	defer upload.stopMutex.Unlock()
	var err error
	if upload.file != nil {
		err = upload.file.Close()
		upload.file = nil
	}
	if upload.isRunning.Load() {
		upload.stop <- struct{}{}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.Wrapf(err, "cannot close upload '%s'", upload.info.ID)
}

func (upload *vfsUpload) SetFile(file io.WriteCloser) error {
	if upload.file != nil {
		return errors.New("file already set")
	}
	upload.file = file
	upload.lastAccess.Store(time.Now())
	return nil
}

func (upload *vfsUpload) Run() {
	upload.logger.Debug().Msgf("upload '%s' is running", upload.info.ID)
	upload.isRunning.Store(true)
	defer func() {
		upload.isRunning.Store(false)
		upload.logger.Debug().Msgf("upload '%s' is stopped", upload.info.ID)
	}()
	for {
		select {
		case <-upload.stop:
			return
		case <-time.After(1 * time.Minute):
			if time.Since(upload.lastAccess.Load()) > 5*time.Minute {
				upload.Stop()
			}
		}
	}
}

func (upload *vfsUpload) GetInfo(ctx context.Context) (handler.FileInfo, error) {
	return upload.info, nil
}

func (upload *vfsUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	defer func() { upload.lastAccess.Store(time.Now()) }()
	if upload.file == nil {
		return 0, errors.New("file is nil")
	}
	n, err := io.Copy(upload.file, src)
	upload.info.Offset += n
	if err != nil {
		return n, errors.Wrapf(err, "failed to write chung to file '%s'", upload.binPath)
	}
	return n, nil
}

func (upload *vfsUpload) GetReader(ctx context.Context) (io.ReadCloser, error) {
	return upload.store.fsys.Open(upload.binPath)
}

func (upload *vfsUpload) Terminate(ctx context.Context) error {
	upload.logger.Debug().Msgf("terminate upload '%s'", upload.info.ID)
	defer func() { upload.store.uploadCache.Remove(upload.info.ID) }()
	var errs = []error{}
	if err := upload.Stop(); err != nil {
		errs = append(errs, errors.Wrapf(err, "cannot close file '%s'", upload.binPath))
	}

	err := writefs.Remove(upload.store.fsys, upload.binPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, errors.Wrapf(err, "cannot remove file '%s'", upload.binPath))
	}

	return errors.Combine(errs...)
}

func (upload *vfsUpload) ConcatUploads(ctx context.Context, uploads []handler.Upload) (err error) {
	for _, partialUpload := range uploads {
		fileUpload := partialUpload.(*vfsUpload)

		src, err := upload.store.fsys.Open(fileUpload.binPath)
		if err != nil {
			src.Close()
			return err
		}

		if _, err := io.Copy(upload.file, src); err != nil {
			src.Close()
			return err
		}
		if err := src.Close(); err != nil {
			return errors.Wrapf(err, "failed to close file '%s'", fileUpload.binPath)
		}
	}

	return
}

func (upload *vfsUpload) DeclareLength(ctx context.Context, length int64) error {
	upload.info.Size = length
	upload.info.SizeIsDeferred = false
	return nil
}

func (upload *vfsUpload) FinishUpload(ctx context.Context) error {
	upload.logger.Debug().Msgf("finish upload '%s'", upload.info.ID)
	defer func() {
		upload.store.uploadCache.Remove(upload.info.ID)
	}()
	if err := upload.Stop(); err != nil {
		return errors.Wrapf(err, "cannot stop upload '%s'", upload.info.ID)
	}
	return nil
}

var _ handler.Upload = &vfsUpload{}
