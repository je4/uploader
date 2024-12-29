package config

import "embed"

//go:embed uploader.toml
var ConfigFS embed.FS
