package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"syscall"
	"time"

	health "github.com/AppsFlyer/go-sundheit"
	healthhttp "github.com/AppsFlyer/go-sundheit/http"
	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-slog/otelslog"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	slogmulti "github.com/samber/slog-multi"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.20.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/openmeterio/openmeter/config"
	"github.com/openmeterio/openmeter/internal/credit"
	nope_credit "github.com/openmeterio/openmeter/internal/credit/nope_connector"
	postgres_credit "github.com/openmeterio/openmeter/internal/credit/postgres_connector"
	"github.com/openmeterio/openmeter/internal/credit/postgres_connector/ent/db"
	"github.com/openmeterio/openmeter/internal/ingest"
	"github.com/openmeterio/openmeter/internal/ingest/ingestdriver"
	"github.com/openmeterio/openmeter/internal/ingest/kafkaingest"
	"github.com/openmeterio/openmeter/internal/ingest/kafkaingest/serializer"
	"github.com/openmeterio/openmeter/internal/meter"
	"github.com/openmeterio/openmeter/internal/namespace"
	"github.com/openmeterio/openmeter/internal/namespace/namespacedriver"
	"github.com/openmeterio/openmeter/internal/server"
	"github.com/openmeterio/openmeter/internal/server/authenticator"
	"github.com/openmeterio/openmeter/internal/server/router"
	"github.com/openmeterio/openmeter/internal/streaming"
	"github.com/openmeterio/openmeter/internal/streaming/clickhouse_connector"
	"github.com/openmeterio/openmeter/pkg/contextx"
	"github.com/openmeterio/openmeter/pkg/errorsx"
	"github.com/openmeterio/openmeter/pkg/framework/operation"
	"github.com/openmeterio/openmeter/pkg/gosundheit"
	pkgkafka "github.com/openmeterio/openmeter/pkg/kafka"
	kafkametrics "github.com/openmeterio/openmeter/pkg/kafka/metrics"
	"github.com/openmeterio/openmeter/pkg/models"
	"github.com/openmeterio/openmeter/pkg/slicesx"
)

const (
	defaultShutdownTimeout = 5 * time.Second
	otelName               = "openmeter.io/backend"
)

func main() {
	v, flags := viper.New(), pflag.NewFlagSet("OpenMeter", pflag.ExitOnError)
	ctx := context.Background()

	config.Configure(v, flags)

	flags.String("config", "", "Configuration file")
	flags.Bool("version", false, "Show version information")

	_ = flags.Parse(os.Args[1:])

	if v, _ := flags.GetBool("version"); v {
		fmt.Printf("%s version %s (%s) built on %s\n", "Open Meter", version, revision, revisionDate)

		os.Exit(0)
	}

	if c, _ := flags.GetString("config"); c != "" {
		v.SetConfigFile(c)
	}

	err := v.ReadInConfig()
	if err != nil && !errors.As(err, &viper.ConfigFileNotFoundError{}) {
		panic(err)
	}

	var conf config.Configuration
	err = v.Unmarshal(&conf, viper.DecodeHook(config.DecodeHook()))
	if err != nil {
		panic(err)
	}

	err = conf.Validate()
	if err != nil {
		panic(err)
	}

	extraResources, _ := resource.New(
		context.Background(),
		resource.WithContainer(),
		resource.WithAttributes(
			semconv.ServiceName("openmeter"),
			semconv.ServiceVersion(version),
			semconv.DeploymentEnvironment(conf.Environment),
		),
	)
	res, _ := resource.Merge(
		resource.Default(),
		extraResources,
	)

	logger := slog.New(slogmulti.Pipe(
		otelslog.NewHandler,
		contextx.NewLogHandler,
		operation.NewLogHandler,
	).Handler(conf.Telemetry.Log.NewHandler(os.Stdout)))
	logger = otelslog.WithResource(logger, res)

	slog.SetDefault(logger)

	telemetryRouter := chi.NewRouter()
	telemetryRouter.Mount("/debug", middleware.Profiler())

	// Initialize OTel Metrics
	otelMeterProvider, err := conf.Telemetry.Metrics.NewMeterProvider(ctx, res)
	if err != nil {
		logger.Error("failed to initialize OpenTelemetry Metrics provider", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer func() {
		// Use dedicated context with timeout for shutdown as parent context might be canceled
		// by the time the execution reaches this stage.
		ctx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
		defer cancel()

		if err := otelMeterProvider.Shutdown(ctx); err != nil {
			logger.Error("shutting down meter provider", slog.String("error", err.Error()))
		}
	}()
	otel.SetMeterProvider(otelMeterProvider)
	metricMeter := otelMeterProvider.Meter(otelName)

	if conf.Telemetry.Metrics.Exporters.Prometheus.Enabled {
		telemetryRouter.Handle("/metrics", promhttp.Handler())
	}

	// Initialize OTel Tracer
	otelTracerProvider, err := conf.Telemetry.Trace.NewTracerProvider(ctx, res)
	if err != nil {
		logger.Error("failed to initialize OpenTelemetry Trace provider", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer func() {
		// Use dedicated context with timeout for shutdown as parent context might be canceled
		// by the time the execution reaches this stage.
		ctx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
		defer cancel()

		if err := otelTracerProvider.Shutdown(ctx); err != nil {
			logger.Error("shutting down tracer provider", slog.String("error", err.Error()))
		}
	}()

	otel.SetTracerProvider(otelTracerProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Configure health checker
	healthChecker := health.New(health.WithCheckListeners(gosundheit.NewLogger(logger.With(slog.String("component", "healthcheck")))))
	{
		handler := healthhttp.HandleHealthJSON(healthChecker)
		telemetryRouter.Handle("/healthz", handler)

		// Kubernetes style health checks
		telemetryRouter.HandleFunc("/healthz/live", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		})
		telemetryRouter.Handle("/healthz/ready", handler)
	}

	logger.Info("starting OpenMeter server", "config", map[string]string{
		"address":             conf.Address,
		"telemetry.address":   conf.Telemetry.Address,
		"ingest.kafka.broker": conf.Ingest.Kafka.Broker,
	})

	var group run.Group
	var ingestCollector ingest.Collector
	var streamingConnector streaming.Connector
	var namespaceHandlers []namespace.Handler

	// Initialize ClickHouse Client
	clickHouseClient, err := initClickHouseClient(conf)
	if err != nil {
		logger.Error("failed to initialize clickhouse client", "error", err)
		os.Exit(1)
	}

	// Initialize Kafka Ingest
	ingestCollector, kafkaIngestNamespaceHandler, err := initKafkaIngest(
		ctx,
		conf,
		logger,
		metricMeter,
		serializer.NewJSONSerializer(),
		&group,
	)
	if err != nil {
		logger.Error("failed to initialize kafka ingest", "error", err)
		os.Exit(1)
	}
	namespaceHandlers = append(namespaceHandlers, kafkaIngestNamespaceHandler)
	defer ingestCollector.Close()

	meterRepository := meter.NewInMemoryRepository(slicesx.Map(conf.Meters, func(meter *models.Meter) models.Meter {
		return *meter
	}))

	// Initialize ClickHouse Aggregation
	clickhouseStreamingConnector, err := initClickHouseStreaming(conf, clickHouseClient, meterRepository, logger)
	if err != nil {
		logger.Error("failed to initialize clickhouse aggregation", "error", err)
		os.Exit(1)
	}

	streamingConnector = clickhouseStreamingConnector
	namespaceHandlers = append(namespaceHandlers, clickhouseStreamingConnector)

	// Initialize Namespace
	namespaceManager, err := initNamespace(conf, namespaceHandlers...)
	if err != nil {
		logger.Error("failed to initialize namespace", "error", err)
		os.Exit(1)
	}

	// ingestHandler, err := httpingest.NewHandler(httpingest.HandlerConfig{
	// 	Collector:        ingestCollector,
	// 	NamespaceManager: namespaceManager,
	// 	Logger:           logger,
	// 	ErrorHandler:     errorsx.NewAppHandler(errorsx.NewSlogHandler(logger)),
	// })
	// if err != nil {
	// 	logger.Error("failed to initialize http ingest handler", "error", err)
	// 	os.Exit(1)
	// }

	// Initialize deduplication
	if conf.Dedupe.Enabled {
		deduplicator, err := conf.Dedupe.NewDeduplicator()
		if err != nil {
			logger.Error("failed to initialize deduplicator", "error", err)
			os.Exit(1)
		}

		ingestCollector = ingest.DeduplicatingCollector{
			Collector:    ingestCollector,
			Deduplicator: deduplicator,
		}
	}

	// Initialize HTTP Ingest handler
	ingestService := ingest.Service{
		Collector: ingestCollector,
		Logger:    logger,
	}
	ingestHandler := ingestdriver.NewIngestEventsHandler(
		ingestService.IngestEvents,
		namespacedriver.StaticNamespaceDecoder(namespaceManager.GetDefaultNamespace()),
		nil,
		errorsx.NewContextHandler(errorsx.NewAppHandler(errorsx.NewSlogHandler(logger))),
	)

	// Initialize portal
	var portalTokenStrategy *authenticator.PortalTokenStrategy
	if conf.Portal.Enabled {
		portalTokenStrategy, err = authenticator.NewPortalTokenStrategy(conf.Portal.TokenSecret, conf.Portal.TokenExpiration)
		if err != nil {
			logger.Error("failed to initialize portal token strategy", "error", err)
			os.Exit(1)
		}
	}

	// Initialize Postgres
	var creditDbClient *db.Client
	if conf.Postgres.URL != "" {
		creditDbClient, err = postgres_credit.Open(conf.Postgres.URL)
		if err != nil {
			logger.Error("failed to open postgres connection", "error", err)
			os.Exit(1)
		}

		// TODO: use versioned migrations
		// https://entgo.io/docs/versioned-migrations
		if err := creditDbClient.Schema.Create(context.Background()); err != nil {
			logger.Error("failed to migrate database", "error", err)
			os.Exit(1)
		}
	}

	// Initialize Credit
	var creditConnector credit.Connector
	if conf.Entitlements.Enabled {
		creditConnector = postgres_credit.NewPostgresConnector(
			logger,
			creditDbClient,
			streamingConnector,
			meterRepository,
			postgres_credit.PostgresConnectorConfig{
				WindowSize: time.Minute,
			},
		)
		if err != nil {
			logger.Error("failed to initialize entitlements support", "error", err)
			os.Exit(1)
		}
		logger.Info("entitlements support enabled")
	} else {
		creditConnector = nope_credit.NewConnector()
		logger.Info("entitlements support disabled")
	}

	s, err := server.NewServer(&server.Config{
		RouterConfig: router.Config{
			CreditConnector:     creditConnector,
			NamespaceManager:    namespaceManager,
			StreamingConnector:  streamingConnector,
			IngestHandler:       ingestHandler,
			Meters:              meterRepository,
			PortalTokenStrategy: portalTokenStrategy,
			PortalCORSEnabled:   conf.Portal.CORS.Enabled,
			ErrorHandler:        errorsx.NewAppHandler(errorsx.NewSlogHandler(logger)),
		},
		RouterHook: func(r chi.Router) {
			r.Use(func(h http.Handler) http.Handler {
				return otelhttp.NewHandler(
					http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						h.ServeHTTP(w, r)

						routePattern := chi.RouteContext(r.Context()).RoutePattern()

						span := trace.SpanFromContext(r.Context())
						span.SetName(routePattern)
						span.SetAttributes(semconv.HTTPTarget(r.URL.String()), semconv.HTTPRoute(routePattern))

						labeler, ok := otelhttp.LabelerFromContext(r.Context())
						if ok {
							labeler.Add(semconv.HTTPRoute(routePattern))
						}
					}),
					"",
					otelhttp.WithMeterProvider(otelMeterProvider),
					otelhttp.WithTracerProvider(otelTracerProvider),
				)
			})
		},
	})
	if err != nil {
		logger.Error("failed to create server", "error", err)
		os.Exit(1)
	}

	s.Get("/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"version": version,
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
		})
	})

	for _, meter := range conf.Meters {
		err := streamingConnector.CreateMeter(ctx, namespaceManager.GetDefaultNamespace(), meter)
		if err != nil {
			slog.Warn("failed to initialize meter", "error", err)
			os.Exit(1)
		}
	}
	slog.Info("meters successfully created", "count", len(conf.Meters))

	// Set up telemetry server
	{
		server := &http.Server{
			Addr:    conf.Telemetry.Address,
			Handler: telemetryRouter,
		}
		defer server.Close()

		group.Add(
			func() error { return server.ListenAndServe() },
			func(err error) { _ = server.Shutdown(ctx) },
		)
	}

	// Set up server
	{
		server := &http.Server{
			Addr:    conf.Address,
			Handler: s,
		}
		defer server.Close()

		group.Add(
			func() error { return server.ListenAndServe() },
			func(err error) { _ = server.Shutdown(ctx) }, // TODO: context deadline
		)
	}

	// Setup signal handler
	group.Add(run.SignalHandler(ctx, syscall.SIGINT, syscall.SIGTERM))

	err = group.Run()
	if e := (run.SignalError{}); errors.As(err, &e) {
		slog.Info("received signal; shutting down", slog.String("signal", e.Signal.String()))
	} else if !errors.Is(err, http.ErrServerClosed) {
		logger.Error("application stopped due to error", slog.String("error", err.Error()))
	}
}

func initKafkaIngest(ctx context.Context, config config.Configuration, logger *slog.Logger, metricMeter metric.Meter, serializer serializer.Serializer, group *run.Group) (*kafkaingest.Collector, *kafkaingest.NamespaceHandler, error) {
	// Initialize Kafka Admin Client
	kafkaConfig := config.Ingest.Kafka.CreateKafkaConfig()

	// Initialize Kafka Producer
	producer, err := kafka.NewProducer(&kafkaConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("init kafka ingest: %w", err)
	}

	// Initialize Kafka Client Statistics reporter
	kafkaMetrics, err := kafkametrics.New(metricMeter)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create Kafka client metrics: %w", err)
	}

	// TODO: move kafkaingest.KafkaProducerGroup to pkg/kafka
	group.Add(kafkaingest.KafkaProducerGroup(ctx, producer, logger, kafkaMetrics))

	go pkgkafka.ConsumeLogChannel(producer, logger.WithGroup("kafka").WithGroup("producer"))

	slog.Debug("connected to Kafka")

	collector, err := kafkaingest.NewCollector(
		producer,
		serializer,
		config.Ingest.Kafka.EventsTopicTemplate,
		metricMeter,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("init kafka ingest: %w", err)
	}

	kafkaAdminClient, err := kafka.NewAdminClientFromProducer(producer)
	if err != nil {
		return nil, nil, err
	}

	namespaceHandler := &kafkaingest.NamespaceHandler{
		AdminClient:             kafkaAdminClient,
		NamespacedTopicTemplate: config.Ingest.Kafka.EventsTopicTemplate,
		Partitions:              config.Ingest.Kafka.Partitions,
		Logger:                  logger,
	}

	return collector, namespaceHandler, nil
}

func initClickHouseClient(config config.Configuration) (clickhouse.Conn, error) {
	options := &clickhouse.Options{
		Addr: []string{config.Aggregation.ClickHouse.Address},
		Auth: clickhouse.Auth{
			Database: config.Aggregation.ClickHouse.Database,
			Username: config.Aggregation.ClickHouse.Username,
			Password: config.Aggregation.ClickHouse.Password,
		},
		DialTimeout:      time.Duration(10) * time.Second,
		MaxOpenConns:     5,
		MaxIdleConns:     5,
		ConnMaxLifetime:  time.Duration(10) * time.Minute,
		ConnOpenStrategy: clickhouse.ConnOpenInOrder,
		BlockBufferSize:  10,
	}
	// This minimal TLS.Config is normally sufficient to connect to the secure native port (normally 9440) on a ClickHouse server.
	// See: https://clickhouse.com/docs/en/integrations/go#using-tls
	if config.Aggregation.ClickHouse.TLS {
		options.TLS = &tls.Config{}
	}

	// Initialize ClickHouse
	clickHouseClient, err := clickhouse.Open(options)
	if err != nil {
		return nil, fmt.Errorf("init clickhouse client: %w", err)
	}

	return clickHouseClient, nil
}

func initClickHouseStreaming(config config.Configuration, clickHouseClient clickhouse.Conn, meterRepository meter.Repository, logger *slog.Logger) (*clickhouse_connector.ClickhouseConnector, error) {
	streamingConnector, err := clickhouse_connector.NewClickhouseConnector(clickhouse_connector.ClickhouseConnectorConfig{
		Logger:               logger,
		ClickHouse:           clickHouseClient,
		Database:             config.Aggregation.ClickHouse.Database,
		Meters:               meterRepository,
		CreateOrReplaceMeter: config.Aggregation.CreateOrReplaceMeter,
		PopulateMeter:        config.Aggregation.PopulateMeter,
	})
	if err != nil {
		return nil, fmt.Errorf("init clickhouse streaming: %w", err)
	}

	return streamingConnector, nil
}

func initNamespace(config config.Configuration, namespaces ...namespace.Handler) (*namespace.Manager, error) {
	namespaceManager, err := namespace.NewManager(namespace.ManagerConfig{
		Handlers:          namespaces,
		DefaultNamespace:  config.Namespace.Default,
		DisableManagement: config.Namespace.DisableManagement,
	})
	if err != nil {
		return nil, fmt.Errorf("create namespace manager: %v", err)
	}

	slog.Debug("create default namespace")
	err = namespaceManager.CreateDefaultNamespace(context.Background())
	if err != nil {
		return nil, fmt.Errorf("create default namespace: %v", err)
	}
	slog.Info("default namespace created")
	return namespaceManager, nil
}
