package main

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/ardanlabs/conf"
	"github.com/tcmhoang/sservices/app/services/sales-api/handlers"
	"github.com/tcmhoang/sservices/business/sys/auth"
	"github.com/tcmhoang/sservices/business/sys/database"
	"github.com/tcmhoang/sservices/foundation/keystore"
	"go.uber.org/automaxprocs/maxprocs"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var build = "develop"

func main() {
	logger, err := initLogger()
	if err != nil {
		log.Printf("Error constructing logger: %s", err)
		os.Exit(1)
	}
	defer logger.Sync()

	if err := run(logger); err != nil {
		logger.Errorw("Startup", "ERROR", err)
		os.Exit(1)
	}

}

func run(log *zap.SugaredLogger) error {

	if _, err := maxprocs.Set(); err != nil {
		return fmt.Errorf("maxprocs %w", err)
	}

	g := runtime.GOMAXPROCS(0)
	log.Infof("Starting service build[%s] CPU[%d]\n", build, g)
	defer log.Infoln("Shutdown complete")

	// TODO: Need to figure out timeouts for http service
	cfg := struct {
		conf.Version
		Web struct {
			APIHOST         string        `conf:"default:0.0.0.0:3000"`
			DebugHost       string        `conf:"default:0.0.0.0:4000"`
			ReadTimeout     time.Duration `conf:"default:5s"`
			WriteTimeout    time.Duration `conf:"default:10s"`
			IdleTimeout     time.Duration `conf:"default:120s"`
			ShutdownTimeout time.Duration `conf:"default:20s"`
		}
		Auth struct {
			KeysFolder string `conf:"default:zarf/keys/"`
			ActiveKID  string `conf:"default:private"`
		}
		DB struct {
			User         string `conf:"default:postgres"`
			Password     string `conf:"default:postgres,mask"`
			Host         string `conf:"default:localhost"`
			Name         string `conf:"default:postgres"`
			MaxIdleConns int    `conf:"default:2"`
			MaxOpenConns int    `conf:"default:0"`
			DisableTLS   bool   `conf:"default:true"`
		}
	}{
		Version: conf.Version{
			SVN:  build,
			Desc: "TCMHOANG",
		},
	}

	confout, err := initConfig(&cfg)

	if err != nil {
		return err
	}

	if err == nil && confout == "" { // help option
		return nil
	}

	log.Infow("Startup", "CONFIG", confout)
	expvar.NewString("build").Set(build)

	ks, err := keystore.NewFS(os.DirFS(cfg.Auth.KeysFolder))
	if err != nil {
		return fmt.Errorf("reading keys: %w", err)
	}

	auth, err := auth.New(cfg.Auth.ActiveKID, ks)
	if err != nil {
		return fmt.Errorf("constructing auth: %w", err)
	}

	log.Infow("startup", "status", "initializing database support", "host", cfg.DB.Host)

	db, err := database.Open(
		database.Config{
			User:         cfg.DB.User,
			Password:     cfg.DB.Password,
			Host:         cfg.DB.Host,
			Name:         cfg.DB.Name,
			MaxIdleConns: cfg.DB.MaxIdleConns,
			MaxOpenConns: cfg.DB.MaxOpenConns,
			DisableTLS:   cfg.DB.DisableTLS,
		},
	)

	if err != nil {
		return fmt.Errorf("connecting to db: %w", err)
	}
	defer func() {
		log.Infow("shutdown", "status", "stopping database support", "host", cfg.DB.Host)
		db.Close()
	}()

	log.Infow("startup", "status", "debug router started", "host", cfg.Web.DebugHost)
	initDebugMux(log, cfg.Web.DebugHost)

	log.Infow("startup", "status", "initializing API support")

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	apiMux := handlers.APIMux(
		handlers.APIMuxConfig{
			Shutdown: shutdown,
			Log:      log,
			Auth:     auth,
		})

	api := http.Server{
		Addr:         cfg.Web.APIHOST,
		Handler:      apiMux,
		ReadTimeout:  cfg.Web.ReadTimeout,
		WriteTimeout: cfg.Web.WriteTimeout,
		IdleTimeout:  cfg.Web.IdleTimeout,
		ErrorLog:     zap.NewStdLog(log.Desugar()),
	}

	severErrs := make(chan error, 1)
	go func() {
		log.Infow("startup", "status", "api router started", "host", api.Addr)
		severErrs <- api.ListenAndServe()
	}()

	select {
	case err := <-severErrs:
		return fmt.Errorf("server error: %w", err)

	case sig := <-shutdown:
		log.Infow("shutdown", "status", "shutdown started", "signal", sig)
		defer log.Infow("shutdown", "status", "shutdown complete", "signal", sig)

		ctx, cancel := context.WithTimeout(context.Background(), cfg.Web.ShutdownTimeout)
		defer cancel()

		if err := api.Shutdown(ctx); err != nil {
			api.Close()
			return fmt.Errorf("could not stop server gracefully: %w", err)
		}
	}

	return nil
}

func initDebugMux(log *zap.SugaredLogger, host string) {
	debugMux := handlers.DebugMux(build, log)

	go func() {
		if err := http.ListenAndServe(host, debugMux); err != nil {
			log.Errorw("shutdown", "status", "debug router closed", "host", host, "ERROR", err)
		}
	}()

}

func initConfig(cfg interface{}) (string, error) {
	const prefix = "SALES"

	help, err := conf.ParseOSArgs(prefix, cfg)
	if err != nil {
		if errors.Is(err, conf.ErrHelpWanted) {
			fmt.Println(help)
			return "", nil
		}
		return "", fmt.Errorf("passing config: %w", err)
	}
	out, err := conf.String(cfg)
	if err != nil {
		return "", fmt.Errorf("generating config for stdout: %w", err)
	}

	return out, nil
}

func initLogger() (*zap.SugaredLogger, error) {
	config := zap.NewProductionConfig()
	config.OutputPaths = []string{"stdout"}
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	config.DisableStacktrace = true
	config.InitialFields = map[string]interface{}{
		"service": "SALES-API",
	}

	log, err := config.Build()
	if err != nil {
		return nil, err
	}

	return log.Sugar(), nil
}
