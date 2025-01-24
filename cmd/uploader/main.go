package main

import (
	"crypto/tls"
	"emperror.dev/errors"
	"flag"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/je4/filesystem/v3/pkg/vfsrw"
	"github.com/je4/uploader/config"
	"github.com/je4/uploader/data/static"
	"github.com/je4/uploader/pkg/vfsstore"
	"github.com/je4/utils/v2/pkg/zLogger"
	tusd "github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"
	ublogger "gitlab.switch.ch/ub-unibas/go-ublogger/v2"
	"go.ub.unibas.ch/cloud/certloader/v2/pkg/loader"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

var configfile = flag.String("config", "", "location of toml configuration file")

func main() {
	flag.Parse()

	var cfgFS fs.FS
	var cfgFile string
	if *configfile != "" {
		cfgFS = os.DirFS(filepath.Dir(*configfile))
		cfgFile = filepath.Base(*configfile)
	} else {
		cfgFS = config.ConfigFS
		cfgFile = "uploader.toml"
	}

	conf := &UploaderMainConfig{
		LocalAddr: "localhost:8443",
		//ResolverTimeout: config.Duration(10 * time.Minute),
		ExternalAddr: "https://localhost:8443",
		LogLevel:     "DEBUG",
	}
	if err := LoadUploaderMainConfig(cfgFS, cfgFile, conf); err != nil {
		log.Fatalf("cannot load toml from [%v] %s: %v", cfgFS, cfgFile, err)
	}

	var loggerTLSConfig *tls.Config
	var loggerLoader io.Closer
	var err error
	if conf.Log.Stash.TLS != nil {
		loggerTLSConfig, loggerLoader, err = loader.CreateClientLoader(conf.Log.Stash.TLS, nil)
		if err != nil {
			log.Fatalf("cannot create client loader: %v", err)
		}
		defer loggerLoader.Close()
	}

	_logger, _logstash, _logfile, err := ublogger.CreateUbMultiLoggerTLS(conf.Log.Level, conf.Log.File,
		ublogger.SetDataset(conf.Log.Stash.Dataset),
		ublogger.SetLogStash(conf.Log.Stash.LogstashHost, conf.Log.Stash.LogstashPort, conf.Log.Stash.Namespace, conf.Log.Stash.LogstashTraceLevel),
		ublogger.SetTLS(conf.Log.Stash.TLS != nil),
		ublogger.SetTLSConfig(loggerTLSConfig),
	)
	if err != nil {
		log.Fatalf("cannot create logger: %v", err)
	}
	if _logstash != nil {
		defer _logstash.Close()
	}
	if _logfile != nil {
		defer _logfile.Close()
	}
	// create logger instance
	hostname, err := os.Hostname()
	if err != nil {
		log.Fatalf("cannot get hostname: %v", err)
	}

	l2 := _logger.With().Timestamp().Str("host", hostname).Str("addr", conf.LocalAddr).Logger() //.Output(output)
	var logger zLogger.ZLogger = &l2

	webTLSConfig, webLoader, err := loader.CreateServerLoader(false, conf.WebTLS, nil, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("cannot create server loader")
	}
	defer webLoader.Close()

	vfs, err := vfsrw.NewFS(conf.VFS, logger)
	if err != nil {
		logger.Panic().Err(err).Msg("cannot create vfs")
	}
	defer func() {
		if err := vfs.Close(); err != nil {
			logger.Error().Err(err).Msg("cannot close vfs")
		}
	}()

	// A storage backend for tusd may consist of multiple different parts which
	// handle upload creation, locking, termination and so on. The composer is a
	// place where all those separated pieces are joined together. In this example
	// we only use the file store but you may plug in multiple.
	composer := tusd.NewStoreComposer()

	// Create a new FileStore instance which is responsible for
	// storing the uploaded file on disk in the specified directory.
	// This path _must_ exist before tusd will store uploads in it.
	// If you want to save them on a different medium, for example
	// a remote FTP server, you can implement your own storage backend
	// by implementing the tusd.DataStore interface.
	store := vfsstore.New(vfs, conf.DefaultUploadPath, conf.AllowedUploadPaths, time.Duration(conf.MaxUploadTime), logger)
	store.UseIn(composer)

	//s3store := s3store.New(conf.S3, logger)

	// A locking mechanism helps preventing data loss or corruption from
	// parallel requests to a upload resource. A good match for the disk-based
	// storage is the memorylocker package which uses memory to store the locks.
	// More information is available at https://tus.github.io/tusd/advanced-topics/locks/.
	locker := memorylocker.New()
	locker.UseIn(composer)

	// Create a new HTTP handler for the tusd server by providing a configuration.
	// The StoreComposer property must be set to allow the handler to function.
	//slogger := slog.New(slogzerolog.Option{Level: slog.LevelDebug, Logger: _logger.Logger}.NewZerologHandler())
	handler, err := tusd.NewUnroutedHandler(tusd.Config{
		BasePath:              "/files/",
		StoreComposer:         composer,
		NotifyCompleteUploads: true,
		//Logger:                slogger,
		DisableDownload: true,
		PreUploadCreateCallback: func(hook tusd.HookEvent) (tusd.HTTPResponse, tusd.FileInfoChanges, error) {
			var resp = tusd.HTTPResponse{
				StatusCode: 0,
				Body:       "",
				Header:     nil,
			}
			var fic = tusd.FileInfoChanges{
				ID:       "",
				MetaData: nil,
				Storage:  map[string]string{},
			}
			var defaultPath string
			if filename := hook.Upload.MetaData["filename"]; filename != "" {
				defaultPath = filename
			}
			if basePath := hook.Upload.MetaData["basePath"]; basePath != "" && defaultPath != "" {
				defaultPath = vfsstore.VFSPathJoin(basePath, defaultPath)
			}
			if defaultPath != "" {
				fic.Storage["Path"] = defaultPath
			}
			return resp, fic, nil
		},
	})
	if err != nil {
		log.Fatalf("unable to create handler: %s", err)
	}

	// Start another goroutine for receiving events from the handler whenever
	// an upload is completed. The event will contains details about the upload
	// itself and the relevant HTTP request.
	go func() {
		for {
			event := <-handler.CompleteUploads
			log.Printf("Upload %s finished\n", event.Upload.ID)
		}
	}()

	router := gin.Default()
	router.Use(cors.Default())
	router.StaticFS("/static", http.FS(static.FS))
	router.POST("/files/", gin.WrapF(handler.PostFile))
	router.HEAD("/files/:id", gin.WrapF(handler.HeadFile))
	router.PATCH("/files/:id", gin.WrapF(handler.PatchFile))
	router.GET("/files/:id", gin.WrapF(handler.GetFile))

	server := http.Server{
		Addr:      conf.LocalAddr,
		Handler:   router,
		TLSConfig: webTLSConfig,
	}

	logger.Info().Msgf("starting server at https://%s\n", conf.LocalAddr)
	logger.Info().Msgf("starting server at %s\n", conf.ExternalAddr)
	if err := server.ListenAndServeTLS("", ""); !errors.Is(err, http.ErrServerClosed) {
		// unexpected error. port in use?
		logger.Error().Err(err).Msgf("server on '%s' ended: %v", conf.LocalAddr, err)
	}

}
