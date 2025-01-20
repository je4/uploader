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
	"github.com/je4/filesystem/v3/pkg/writefs"
	"github.com/tus/tusd/v2/pkg/handler"
	"io"
	"time"
)

func newUpload(info handler.FileInfo, binPath string, fp writefs.FileWrite, store *VFSStore) *fileUpload {
	upload := &fileUpload{
		info:            info,
		binPath:         binPath,
		fp:              fp,
		store:           store,
		resetCloseTimer: make(chan struct{}),
		endCloseTimer:   make(chan struct{}),
	}
	upload.AutoCloser()
	store.addUpload(upload)
	return upload
}

type fileUpload struct {
	// info stores the current information about the upload
	info handler.FileInfo
	// binPath is the path to the binary file (which has no extension)
	binPath         string
	fp              writefs.FileWrite
	store           *VFSStore
	resetCloseTimer chan struct{}
	endCloseTimer   chan struct{}
}

func (upload *fileUpload) AutoCloser() {
	go func() {
		for {
			select {
			case <-time.After(upload.store.maxUploadTime):
				upload.store.logger.Warn().Msgf("uploadCloser %s timed out", upload.info.ID)
				upload.store.removeUpload(upload.info.ID)
				if err := upload.Close(); err != nil {
					upload.store.logger.Error().Err(err).Msgf("cannot close upload %s", upload.info.ID)
				}
				return
			case <-upload.resetCloseTimer:
				upload.store.logger.Debug().Msgf("uploadCloser %s reset", upload.info.ID)
				continue
			case <-upload.endCloseTimer:
				upload.store.logger.Debug().Msgf("uploadCloser %s finished", upload.info.ID)
				return
			}
		}
	}()
}

func (upload *fileUpload) GetInfo(ctx context.Context) (handler.FileInfo, error) {
	return upload.info, nil
}

func (upload *fileUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	upload.resetCloseTimer <- struct{}{}
	n, err := io.Copy(upload.fp, src)
	upload.info.Offset += n
	if err != nil {
		upload.Close()
		return n, errors.WithStack(err)
	}

	return n, nil
}

func (upload *fileUpload) GetReader(ctx context.Context) (io.ReadCloser, error) {
	return upload.store.vfs.Open(upload.binPath)
}

func (upload *fileUpload) DeclareLength(ctx context.Context, length int64) error {
	upload.info.Size = length
	upload.info.SizeIsDeferred = false
	return nil
}

func (upload *fileUpload) FinishUpload(ctx context.Context) error {
	upload.endCloseTimer <- struct{}{}
	return errors.Wrapf(upload.Close(), "cannot finish upload %s", upload.info.ID)
}

func (upload *fileUpload) Close() error {
	var errs = []error{}
	upload.store.removeUpload(upload.info.ID)
	if err := upload.fp.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Wrapf(errors.Combine(errs...), "cannot close upload %s", upload.info.ID)
}
