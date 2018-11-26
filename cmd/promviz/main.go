package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
        "io/ioutil"

	"github.com/nghialv/promviz/api"
	"github.com/nghialv/promviz/cache"
	"github.com/nghialv/promviz/config"
	"github.com/nghialv/promviz/retrieval"
	"github.com/nghialv/promviz/storage"
	"github.com/nghialv/promviz/version"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"gopkg.in/alecthomas/kingpin.v2"
        "github.com/matche2100/gabs"
)

var (
	configSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "promviz",
		Name:      "config_last_reload_successful",
		Help:      "Whether the last configuration reload attempt was successful.",
	})
)

func main() {
	cfg := struct {
		configFile  string
		logLevel    string
		storagePath string

                positionFilePath string

		api       api.Options
		retrieval retrieval.Options
		cache     cache.Options
		storage   storage.Options
	}{}

	a := kingpin.New(filepath.Base(os.Args[0]), "The Promviz server")
	a.Version(version.Version)
	a.HelpFlag.Short('h')

	a.Flag("config.file", "Promviz configuration file path.").
		Default("/etc/promviz/promviz.yaml").StringVar(&cfg.configFile)

	a.Flag("log.level", "The level of logging.").
		Default("info").StringVar(&cfg.logLevel)

	a.Flag("api.port", "Port to listen on for API requests.").
		Default("9091").IntVar(&cfg.api.ListenPort)

	a.Flag("retrieval.scrape-interval", "How frequently to scrape metrics from prometheus servers.").
		Default("10s").DurationVar(&cfg.retrieval.ScrapeInterval)

	a.Flag("retrieval.scrape-timeout", "How long until a scrape request times out.").
		Default("8s").DurationVar(&cfg.retrieval.ScrapeTimeout)

	a.Flag("cache.size", "The maximum number of snapshots can be cached.").
		Default("100").IntVar(&cfg.cache.Size)

	a.Flag("storage.path", "Base path of local storage for graph data.").
		Default("/promviz").StringVar(&cfg.storagePath)

	a.Flag("storage.retention", "How long to retain graph data in the storage.").
		Default("168h").DurationVar(&cfg.storage.Retention)

        a.Flag("position.path", "FilePath for Position of nodes.").
                Default("./position.json").StringVar(&cfg.positionFilePath)

	_, err := a.Parse(os.Args[1:])
	if err != nil {
		fmt.Printf("Failed to parse arguments: %v\n", err)
		a.Usage(os.Args[1:])
		os.Exit(2)
	}

	// TODO: log lever
	logger, err := zap.NewProduction()
	if err != nil {
		os.Exit(2)
	}
	defer logger.Sync()

        PositionFile := new(config.PositionFile)
        PositionFile.Path = cfg.positionFilePath


        go func(){

           positionfiledata, err := ioutil.ReadFile(PositionFile.Path)
           positiondata := gabs.New()
           
           if err != nil {
               logger.Info("PositionFile not found. Create New File.")
           } else {
               positiondata2, err := gabs.ParseJSON(positionfiledata)
               if err == nil {
                     logger.Info("PositionFile parsing success.")
                     positiondata.Merge(positiondata2) 
               } else {
                     logger.Info("PositionFile parsing failure. Blank start.")
               }
           }

           PositionFile.PositionData = positiondata

        }()

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		prometheus.NewGoCollector(),
		configSuccess)

	storageReady := make(chan struct{})
	var db storage.Storage
	go func() {
		defer close(storageReady)
		var err error
		db, err = storage.Open(
			cfg.storagePath,
			logger.With(zap.String("component", "storage")),
			registry,
			&cfg.storage,
		)
		if err != nil {
			logger.Error("Failed to open db", zap.Error(err))
			os.Exit(1)
		}
	}()
	<-storageReady
	defer db.Close()

	cfg.api.ConfigFile = cfg.configFile
	cfg.api.Querier = db
	cfg.retrieval.Appender = db

        cfg.retrieval.PositionFile = PositionFile

	retriever := retrieval.NewRetriever(
		logger.With(zap.String("component", "retrieval")),
		registry,
		&cfg.retrieval,
	)

	if err := reloadConfig(cfg.configFile, logger, retriever); err != nil {
		logger.Error("Failed to run first reloading config", zap.Error(err))
	}

	go retriever.Run()
	defer retriever.Stop()

	cache := cache.NewCache(
		logger.With(zap.String("component", "cache")),
		registry,
		&cfg.cache,
	)
	defer cache.Reset()

	cfg.api.Cache = cache

        cfg.api.PositionFile = PositionFile

	apiHandler := api.NewHandler(
		logger.With(zap.String("component", "api")),
		registry,
		&cfg.api,
	)

	logger.Info("Starting promviz", zap.String("info", version.String()))
	errCh := make(chan error)

	go func() {
		if err := apiHandler.Run(registry); err != nil {
			errCh <- err
		}
	}()
	defer apiHandler.Stop()

	go func() {
		for {
			rc := <-apiHandler.Reload()
			if err := reloadConfig(cfg.configFile, logger, retriever); err != nil {
				logger.Error("Failed to reload config", zap.Error(err))
				rc <- err
			} else {
				rc <- nil
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigCh:
		logger.Warn("Received SIGTERM, exiting gracefully...")
	case err := <-errCh:
		logger.Error("Got an error from errCh, exiting gracefully", zap.Error(err))
	}
}

type Reloadable interface {
	ApplyConfig(*config.Config) error
}

func reloadConfig(path string, logger *zap.Logger, rl Reloadable) (err error) {
	logger.Info("Loading configuration file", zap.String("filepath", path))

	defer func() {
		if err != nil {
			configSuccess.Set(0)
		} else {
			configSuccess.Set(1)
		}
	}()

	cfg, err := config.LoadFile(path)
	if err != nil {
		return fmt.Errorf("Failed to load configuration (--config.file=%s): %v", path, err)
	}
	err = rl.ApplyConfig(cfg)
	if err != nil {
		return err
	}
	return nil
}
