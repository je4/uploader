package vfsstore

import (
	"context"
	"emperror.dev/errors"
	"github.com/je4/filesystem/v3/pkg/writefs"
	"github.com/tus/tusd/v2/pkg/handler"
	"io"
	"io/fs"
)

type vfsUpload struct {
	fsys fs.FS
	file io.WriteCloser

	// info stores the current information about the upload
	info handler.FileInfo
	// binPath is the path to the binary file (which has no extension)
	binPath string
}

func (upload *vfsUpload) GetInfo(ctx context.Context) (handler.FileInfo, error) {
	return upload.info, nil
}

func (upload *vfsUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
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
	return upload.fsys.Open(upload.binPath)
}

func (upload *vfsUpload) Terminate(ctx context.Context) error {
	if upload.file == nil {
		return nil
	}

	// We ignore errors indicating that the files cannot be found because we want
	// to delete them anyways. The files might be removed by a cron job for cleaning up
	// or some file might have been removed when tusd crashed during the termination.
	upload.file.Close()
	upload.file = nil

	err := writefs.Remove(upload.fsys, upload.binPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

func (upload *vfsUpload) ConcatUploads(ctx context.Context, uploads []handler.Upload) (err error) {
	for _, partialUpload := range uploads {
		fileUpload := partialUpload.(*vfsUpload)

		src, err := upload.fsys.Open(fileUpload.binPath)
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
	defer func() { upload.file = nil }()
	if err := upload.file.Close(); err != nil {
		return errors.Wrapf(err, "cannot close file '%s'", upload.binPath)
	}
	return nil
}
