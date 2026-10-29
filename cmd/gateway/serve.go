package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
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
	"wopal-codex-gateway/internal/notify"
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

	if err := config.SetProjectRoot(root); err != nil {
		return err
	}
	initialSnapshot, err := loadGatewaySnapshot(root, gatewayConfigPath)
	if err != nil {
		return err
	}

	closeServiceLogs, err := redirectServiceLogs(root, initialSnapshot.gateway.Logs)
	if err != nil {
		return err
	}
	defer closeServiceLogs()

	log.SetOutput(os.Stdout)
	log.AddHook(errorOnlyHook{})

	daemonLogger, err := activity.New(config.LogsDirFor(root), initialSnapshot.gateway.Logs)
	if err != nil {
		return fmt.Errorf("initialize daemon activity logger: %w", err)
	}
	defer daemonLogger.Close()

	reloadStatus := newRuntimeReloadStatus()
	switcher := newBackendSwitcher(nil)
	catalogStatus := newRuntimeCatalogStatus(initialSnapshot.baseline)
	reloadStatus.SetCatalog(catalogStatus)
	prepareInitialCatalog(root, gatewayConfigPath, initialSnapshot, catalogStatus, daemonLogger)
	var recovery *catalogRecoveryController
	recovery = newCatalogRecoveryController(switcher, catalogStatus, func() (*runtimeBackend, error) {
		return recoverCatalogRuntime(root, gatewayConfigPath, *instanceFlag, initialSnapshot.gateway.Listen, daemonLogger, reloadStatus, recovery.Resolve)
	})
	recovery.logger = daemonLogger
	backend, snapshot, err := startGatewayBackendWithLoggerAndResolver(root, gatewayConfigPath, *instanceFlag, daemonLogger, reloadStatus, recovery.Resolve)
	if err != nil {
		return err
	}
	switcher.Swap(backend)
	logBackend(backend, map[string]any{"event": "gateway_started", "architecture": "snapshot-frontdoor", "port": snapshot.gateway.Listen.Port})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	listen := snapshot.gateway.Listen
	front := &http.Server{
		Addr:    net.JoinHostPort(listen.Host, fmt.Sprintf("%d", listen.Port)),
		Handler: switcher,
	}
	frontErr := make(chan error, 1)
	go func() {
		err := front.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		frontErr <- err
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				candidate, reloadErr := reloadGatewayBackendWithResolver(root, gatewayConfigPath, *instanceFlag, listen, daemonLogger, reloadStatus, recovery.Resolve)
				if reloadErr != nil {
					reloadStatus.MarkFailure(reloadErr)
					logBackend(switcher.Current(), map[string]any{"event": "gateway_reload_failed", "error": reloadErr.Error()})
					continue
				}
				switcher.Swap(candidate)
				if candidate.verifier != nil {
					if current, verifyErr := candidate.verifier.Check(); verifyErr == nil {
						catalogStatus.MarkCurrent(candidate.baseline, current.SHA256)
					}
				}
				reloadStatus.MarkSuccess()
				logBackend(candidate, map[string]any{"event": "gateway_reloaded"})
			}
		}
	}()

	select {
	case err := <-frontErr:
		if err != nil {
			logBackend(switcher.Current(), map[string]any{"event": "gateway_failed", "error": err.Error()})
			switcher.Close()
			return fmt.Errorf("run gateway front door: %w", err)
		}
	case <-ctx.Done():
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	_ = front.Shutdown(shutdownCtx)
	logBackend(switcher.Current(), map[string]any{"event": "gateway_stopped"})
	switcher.Close()
	return nil
}

// buildGatewayService assembles the embedded CLIProxyAPI service exactly as the
// daemon runs it. It is separate from runServe so integration tests can drive
// the real assembly — the same server options, routing middleware and status
// handlers — without capturing the process's stdout and stderr.
func buildGatewayService(root string, gateway *config.GatewayConfig, wopalKey string, providerKeys map[string]string, instance string) (*cliproxy.Service, *activity.Logger, error) {
	return buildGatewayServiceWithRuntime(root, gateway, wopalKey, providerKeys, instance, nil, false)
}

func buildGatewayServiceWithRuntime(root string, gateway *config.GatewayConfig, wopalKey string, providerKeys map[string]string, instance string, resolve proxy.RouteResolver, freezeSnapshot bool) (*cliproxy.Service, *activity.Logger, error) {
	return buildGatewayServiceWithRuntimeLogger(root, gateway, wopalKey, providerKeys, instance, resolve, freezeSnapshot, nil)
}

func buildGatewayServiceWithRuntimeLogger(root string, gateway *config.GatewayConfig, wopalKey string, providerKeys map[string]string, instance string, resolve proxy.RouteResolver, freezeSnapshot bool, sharedLogger *activity.Logger, runtimeSets ...gatewayServiceRuntime) (*cliproxy.Service, *activity.Logger, error) {
	// The SDK request logger is disabled at construction time (see
	// disableSDKRequestLogging): no request-logging middleware is installed, so
	// no request or response body can be written to disk.
	requestBodyLogging := requestBodyLoggingState

	compaction.ConfigureKey(wopalKey)
	cliConfig, cliConfigPath, err := config.BuildCLIProxyConfig(root, gateway, wopalKey, providerKeys)
	if err != nil {
		return nil, nil, err
	}
	logger := sharedLogger
	createdLogger := false
	if logger == nil {
		logger, err = activity.New(config.LogsDirFor(root), gateway.Logs)
		if err != nil {
			return nil, nil, fmt.Errorf("initialize activity logger: %w", err)
		}
		createdLogger = true
	}
	logger.Write(map[string]any{
		"event": "gateway_starting", "host": gateway.Listen.Host, "port": gateway.Listen.Port,
		"wopal_models": len(gateway.WopalAI.Models), "relay_port": nil,
	})
	quotaGuard := quota.NewGuardWithState(logger, config.QuotaStatePathFor(root), time.Now())
	quotaGuard.SetNotifier(notify.NewQuotaNotifier(logger, gateway.Notifications))
	logger.Write(map[string]any{"event": "quota_notification_configured", "status": quotaGuard.NotificationStatus()})

	middleware := proxy.RoutingMiddleware(logger, quotaGuard)
	runtimeState := gatewayServiceRuntime{StatusGateway: gateway, Reload: newRuntimeReloadStatus()}
	if len(runtimeSets) > 0 {
		runtimeState = runtimeSets[0]
		if runtimeState.StatusGateway == nil {
			runtimeState.StatusGateway = gateway
		}
		if runtimeState.Reload == nil {
			runtimeState.Reload = newRuntimeReloadStatus()
		}
	}
	if resolve != nil {
		middleware = proxy.RoutingMiddlewareWithResolverAndRoles(logger, quotaGuard, resolve, runtimeState.Roles)
	}
	builder := cliproxy.NewBuilder().
		WithConfig(cliConfig).
		WithConfigPath(cliConfigPath).
		WithServerOptions(
			sdkapi.WithRequestLoggerFactory(disableSDKRequestLogging),
			sdkapi.WithMiddleware(middleware),
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
					c.JSON(200, buildRuntimeStatusPayload(
						runtimeState.StatusGateway,
						runtimeState.Roles,
						runtimeState.Credentials,
						runtimeState.Reload.Snapshot(),
						runtimeState.Catalog.Snapshot(),
						quotaGuard.Status(time.Now()),
						quotaGuard.FailoverStatus(),
						quotaGuard.NotificationStatus(),
						instance,
						requestBodyLogging,
					))
				})
			}),
		)
	if freezeSnapshot {
		builder = builder.WithWatcherFactory(func(string, string, func(*sdkconfig.Config)) (*cliproxy.WatcherWrapper, error) {
			return nil, nil
		})
	}
	service, err := builder.Build()
	if err != nil {
		if createdLogger {
			logger.Close()
		}
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

	stdout, err := captureStream(filepath.Join(logsDirectory, "service.log"), os.Stdout, newServiceLogFilter, cfg)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("redirect stdout: %w", err)
	}
	cleanups = append(cleanups, stdout)

	stderr, err := captureStream(filepath.Join(logsDirectory, "service-error.log"), os.Stderr, func(dst io.Writer) io.Writer {
		return newServiceLogFilter(activity.NewAbortHandlerFilterWriter(dst))
	}, cfg)
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
	if wrap != nil {
		sink = wrap(out)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(sink, reader)
	}()

	return func() {
		_ = reader.Close()
		<-done
		if flusher, ok := sink.(interface{ Flush() }); ok {
			flusher.Flush()
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
