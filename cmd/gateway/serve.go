package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	sdkapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/compaction"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/keychain"
	"wopal-codex-gateway/internal/notify"
	"wopal-codex-gateway/internal/provider"
	"wopal-codex-gateway/internal/proxy"
	"wopal-codex-gateway/internal/quota"
)

// runServe is the only daemon entry point. It owns the listener, the embedded
// CLIProxyAPI service and every long-lived goroutine.
func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	rootFlag := flags.String("project-root", "", "project root containing runtime/ and logs/")
	configFlag := flags.String("config", "", "gateway config path")
	instanceFlag := flags.String("instance", "", "launch-scoped instance id echoed by /_health")
	if err := flags.Parse(args); err != nil {
		return err
	}

	root, err := config.ResolveProjectRoot(*rootFlag)
	if err != nil {
		return err
	}
	gatewayConfigPath := *configFlag
	if gatewayConfigPath == "" {
		gatewayConfigPath = config.ConfigPathFor(root)
	}

	gateway, err := config.LoadGatewayConfig(gatewayConfigPath)
	if err != nil {
		return err
	}
	if err := config.ValidateServeConfig(gateway); err != nil {
		return fmt.Errorf("%s: %w", gatewayConfigPath, err)
	}
	if _, err := provider.Build(gateway); err != nil {
		return fmt.Errorf("%s: %w", gatewayConfigPath, err)
	}
	if err := config.SetProjectRoot(root); err != nil {
		return err
	}
	gateway, err = resolveRuntimeModels(gateway)
	if err != nil {
		return err
	}
	wopalKey, err := keychain.GetWopalAPIKey()
	if err != nil {
		return err
	}
	providerKeys, err := loadRuntimeProviderKeys(gateway)
	if err != nil {
		return err
	}

	closeServiceLogs, err := redirectServiceLogs(root, gateway.Logs)
	if err != nil {
		return err
	}
	defer closeServiceLogs()

	log.SetOutput(os.Stdout)
	log.AddHook(errorOnlyHook{})

	service, logger, err := buildGatewayService(root, gateway, wopalKey, providerKeys, *instanceFlag)
	if err != nil {
		return err
	}
	logger.Write(map[string]any{"event": "gateway_started", "architecture": "embedded-cliproxy", "port": gateway.Listen.Port})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := service.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Write(map[string]any{"event": "gateway_failed", "error": err.Error()})
		return fmt.Errorf("run embedded CLIProxyAPI service: %w", err)
	}
	logger.Write(map[string]any{"event": "gateway_stopped"})
	return nil
}

// buildGatewayService assembles the embedded CLIProxyAPI service exactly as the
// daemon runs it. It is separate from runServe so integration tests can drive
// the real assembly — the same server options, routing middleware and status
// handlers — without capturing the process's stdout and stderr.
func buildGatewayService(root string, gateway *config.GatewayConfig, wopalKey string, providerKeys map[string]string, instance string) (*cliproxy.Service, *activity.Logger, error) {
	// The SDK request logger is disabled at construction time (see
	// disableSDKRequestLogging): no request-logging middleware is installed, so
	// no request or response body can be written to disk.
	requestBodyLogging := requestBodyLoggingState

	compaction.ConfigureKey(wopalKey)
	cliConfig, cliConfigPath, err := config.BuildCLIProxyConfig(root, gateway, wopalKey, providerKeys)
	if err != nil {
		return nil, nil, err
	}
	logger, err := activity.New(config.LogsDirFor(root), gateway.Logs)
	if err != nil {
		return nil, nil, fmt.Errorf("initialize activity logger: %w", err)
	}
	logger.Write(map[string]any{
		"event": "gateway_starting", "host": gateway.Listen.Host, "port": gateway.Listen.Port,
		"wopal_models": len(gateway.WopalAI.Models), "relay_port": nil,
	})
	quotaGuard := quota.NewGuardWithStateAndMaxStep(logger, config.QuotaStatePathFor(root), time.Now(), gateway.Quota.MaxStepCap)
	quotaGuard.SetNotifier(notify.NewQuotaNotifier(logger, gateway.Notifications))
	logger.Write(map[string]any{"event": "quota_notification_configured", "status": quotaGuard.NotificationStatus()})

	builder := cliproxy.NewBuilder().
		WithConfig(cliConfig).
		WithConfigPath(cliConfigPath).
		WithServerOptions(
			sdkapi.WithRequestLoggerFactory(disableSDKRequestLogging),
			sdkapi.WithMiddleware(proxy.RoutingMiddleware(logger, quotaGuard)),
			sdkapi.WithRouterConfigurator(func(engine *gin.Engine, _ *handlers.BaseAPIHandler, _ *sdkconfig.Config) {
				engine.GET("/_health", func(c *gin.Context) {
					c.JSON(200, gin.H{
						"ok":           true,
						"mode":         "single-process",
						"wopal_models": len(gateway.WopalAI.Models),
						"instance":     instance,
					})
				})
				engine.GET("/_status", func(c *gin.Context) {
					c.JSON(200, gin.H{
						"ok":                   true,
						"listen":               fmt.Sprintf("%s:%d", gateway.Listen.Host, gateway.Listen.Port),
						"official":             "native-pass-through",
						"official_quota":       quotaGuard.Status(time.Now()),
						"notifications":        quotaGuard.NotificationStatus(),
						"wopal":                "cliproxy-responses-to-chat",
						"request_body_logging": requestBodyLogging,
					})
				})
			}),
		)
	service, err := builder.Build()
	if err != nil {
		return nil, nil, fmt.Errorf("build embedded CLIProxyAPI service: %w", err)
	}
	return service, logger, nil
}

// redirectServiceLogs sends the embedded service's stdout and stderr to
// size-rotated files. The process keeps writing to an inherited descriptor, so
// each stream is captured through a pipe and copied into a rotating writer;
// handing the file descriptor straight to Dup2 would bypass rotation and keep
// appending to the renamed file forever.
func redirectServiceLogs(root string, cfg config.Logs) (func(), error) {
	logsDirectory := config.LogsDirFor(root)
	if err := os.MkdirAll(logsDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create logs directory: %w", err)
	}

	var cleanups []func()
	closeAll := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	stdout, err := captureStream(filepath.Join(logsDirectory, "service.log"), os.Stdout, nil, cfg)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("redirect stdout: %w", err)
	}
	cleanups = append(cleanups, stdout)

	stderr, err := captureStream(filepath.Join(logsDirectory, "service-error.log"), os.Stderr, activity.NewAbortHandlerFilterWriter, cfg)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("redirect stderr: %w", err)
	}
	cleanups = append(cleanups, stderr)

	return closeAll, nil
}

// captureStream points target at a pipe and copies the pipe into a rotating
// writer in the background. The returned func stops the copy and closes both
// ends so the final bytes are flushed.
func captureStream(path string, target *os.File, wrap func(io.Writer) io.Writer, cfg config.Logs) (func(), error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	if err := syscall.Dup2(int(writer.Fd()), int(target.Fd())); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	_ = writer.Close()

	out := activity.NewRotatingWriter(path, cfg)
	var sink io.Writer = out
	var filtered *activity.AbortHandlerFilterWriter
	if wrap != nil {
		sink = wrap(out)
		filtered, _ = sink.(*activity.AbortHandlerFilterWriter)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(sink, reader)
	}()

	return func() {
		_ = reader.Close()
		<-done
		if filtered != nil {
			filtered.Flush()
		}
		_ = out.Close()
	}, nil
}

type errorOnlyHook struct{}

func (errorOnlyHook) Levels() []log.Level {
	return []log.Level{log.PanicLevel, log.FatalLevel, log.ErrorLevel}
}

func (errorOnlyHook) Fire(entry *log.Entry) error {
	line, err := entry.String()
	if err != nil {
		return err
	}
	_, err = os.Stderr.WriteString(line)
	return err
}
