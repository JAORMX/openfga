package main

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/kelseyhightower/envconfig"
	"github.com/openfga/openfga/pkg/encoder"
	"github.com/openfga/openfga/pkg/logger"
	"github.com/openfga/openfga/pkg/telemetry"
	"github.com/openfga/openfga/server"
	"github.com/openfga/openfga/server/authentication"
	"github.com/openfga/openfga/server/authentication/oidc"
	"github.com/openfga/openfga/server/authentication/presharedkey"
	"github.com/openfga/openfga/server/middleware"
	"github.com/openfga/openfga/storage"
	"github.com/openfga/openfga/storage/caching"
	"github.com/openfga/openfga/storage/memory"
	"github.com/openfga/openfga/storage/postgres"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type svcConfig struct {
	// Optional configuration
	DatastoreEngine               string `default:"memory" split_words:"true" required:"true"`
	DatastoreConnectionURI        string `split_words:"true"`
	DatastoreMaxCacheSize         int    `default:"100000" split_words:"true"`
	ServiceName                   string `default:"openfga" split_words:"true"`
	HTTPPort                      int    `default:"8080" split_words:"true"`
	RPCPort                       int    `default:"8081" split_words:"true"`
	MaxTuplesPerWrite             int    `default:"100" split_words:"true"`
	MaxTypesPerAuthorizationModel int    `default:"100" split_words:"true"`
	// ChangelogHorizonOffset is an offset in minutes from the current time. Changes that occur after this offset will not be included in the response of ReadChanges.
	ChangelogHorizonOffset int `default:"0" split_words:"true" `
	// ResolveNodeLimit indicates how deeply nested an authorization model can be.
	ResolveNodeLimit uint32 `default:"25" split_words:"true"`
	// RequestTimeout is a limit on the time a request may take. If the value is 0, then there is no timeout.
	RequestTimeout time.Duration `default:"0s" split_words:"true"`

	// Authentication. Possible options: none,preshared,oidc
	AuthMethod string `default:"none" split_words:"true"`

	// Shared key authentication
	PresharedKeys []string `default:"" split_words:"true"`

	// OIDC authentication
	IssuerURL string `default:"" split_words:"true"`
	Audience  string `default:"" split_words:"true"`
}

func main() {

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger, err := logger.NewZapLogger()
	if err != nil {
		log.Fatalf("failed to initialize logger: %v", err)
	}

	logger.With(
		zap.String("build.version", version),
		zap.String("build.commit", commit),
	)

	datastore, openFgaServer, err := buildServerAndDatastore(logger)
	if err != nil {
		logger.Fatal("failed to initialize openfga server", zap.Error(err))
	}

	g, ctx := errgroup.WithContext(ctx)

	logger.Info(
		"🚀 starting openfga service...",
		zap.String("version", version),
		zap.String("date", date),
		zap.String("commit", commit),
		zap.String("go-version", runtime.Version()),
	)

	g.Go(func() error {
		return openFgaServer.Run(ctx)
	})

	if err := g.Wait(); err != nil {
		logger.Error("failed to run openfga server", zap.Error(err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := openFgaServer.Close(ctx); err != nil {
		logger.Error("failed to gracefully shutdown openfga server", zap.Error(err))
	}

	if err := datastore.Close(ctx); err != nil {
		logger.Error("failed to gracefully shutdown openfga datastore", zap.Error(err))
	}

	logger.Info("Server exiting. Goodbye 👋")
}

func buildServerAndDatastore(logger logger.Logger) (storage.OpenFGADatastore, *server.Server, error) {
	var config svcConfig
	var err error
	var datastore storage.OpenFGADatastore

	if err := envconfig.Process("OPENFGA", &config); err != nil {
		return nil, nil, fmt.Errorf("failed to process server config: %v", err)
	}

	tracer := telemetry.NewNoopTracer()
	meter := telemetry.NewNoopMeter()
	tokenEncoder := encoder.NewBase64Encoder()

	switch config.DatastoreEngine {
	case "memory":
		logger.Info("using 'memory' storage engine")

		datastore = memory.New(tracer, config.MaxTuplesPerWrite, config.MaxTypesPerAuthorizationModel)
	case "postgres":
		logger.Info("using 'postgres' storage engine")

		opts := []postgres.PostgresOption{
			postgres.WithLogger(logger),
			postgres.WithTracer(tracer),
		}

		datastore, err = postgres.NewPostgresDatastore(config.DatastoreConnectionURI, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to initialize postgres datastore: %v", err)
		}
	default:
		return nil, nil, fmt.Errorf("storage engine '%s' is unsupported", config.DatastoreEngine)
	}

	var interceptors []grpc.UnaryServerInterceptor
	var authenticator authentication.Authenticator

	switch config.AuthMethod {
	case "preshared":
		authenticator, err = presharedkey.NewPresharedKeyAuthenticator(config.PresharedKeys)

	case "oidc":
		authenticator, err = oidc.NewRemoteOidcAuthenticator(config.IssuerURL, config.Audience)
	default:
		err = nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize authenticator: %v", err)
	}

	if authenticator != nil {
		interceptors = append(interceptors, middleware.NewAuthenticationInterceptor(authenticator))
	}

	openFgaServer, err := server.New(&server.Dependencies{
		Datastore:     caching.NewCachedOpenFGADatastore(datastore, config.DatastoreMaxCacheSize),
		Tracer:        tracer,
		Logger:        logger,
		Meter:         meter,
		TokenEncoder:  tokenEncoder,
		Authenticator: authenticator,
	}, &server.Config{
		ServiceName:            config.ServiceName,
		RPCPort:                config.RPCPort,
		HTTPPort:               config.HTTPPort,
		ResolveNodeLimit:       config.ResolveNodeLimit,
		ChangelogHorizonOffset: config.ChangelogHorizonOffset,
		UnaryInterceptors:      interceptors,
		MuxOptions:             nil,
		RequestTimeout:         config.RequestTimeout,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize openfga server: %v", err)
	}

	return datastore, openFgaServer, nil
}
