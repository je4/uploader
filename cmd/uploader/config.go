package main

import (
	"emperror.dev/errors"
	"github.com/BurntSushi/toml"
	"github.com/je4/filesystem/v3/pkg/vfsrw"
	"github.com/je4/utils/v2/pkg/config"
	"github.com/je4/utils/v2/pkg/stashconfig"
	"go.ub.unibas.ch/cloud/certloader/v2/pkg/loader"
	"io/fs"
	"os"
)

type UploaderMainConfig struct {
	LocalAddr          string                `toml:"localaddr"`
	Domain             string                `toml:"domain"`
	ExternalAddr       string                `toml:"externaladdr"`
	WebTLS             *loader.Config        `toml:"webtls"`
	LogFile            string                `toml:"logfile"`
	LogLevel           string                `toml:"loglevel"`
	VFS                map[string]*vfsrw.VFS `toml:"vfs"`
	Log                stashconfig.Config    `toml:"log"`
	DefaultUploadPath  string                `toml:"defaultuploadpath"`
	AllowedUploadPaths []string              `toml:"alloweduploadpaths"`
	MaxUploadTime      config.Duration       `toml:"maxuploadtime"`
}

func LoadUploaderMainConfig(fSys fs.FS, fp string, conf *UploaderMainConfig) error {
	if _, err := fs.Stat(fSys, fp); err != nil {
		path, err := os.Getwd()
		if err != nil {
			return errors.Wrap(err, "cannot get current working directory")
		}
		fSys = os.DirFS(path)
		fp = "uploadermain.toml"
	}
	data, err := fs.ReadFile(fSys, fp)
	if err != nil {
		return errors.Wrapf(err, "cannot read file [%v] %s", fSys, fp)
	}
	_, err = toml.Decode(string(data), conf)
	if err != nil {
		return errors.Wrapf(err, "error loading config file %v", fp)
	}
	return nil
}
