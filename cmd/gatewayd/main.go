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
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "wopal-codex-gateway: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	rootFlag := flag.String("project-root", "", "project root containing runtime/ and logs/")
	configFlag := flag.String("config", "", "gateway config path")
	flag.Parse()

	root, err := resolveProjectRoot(*rootFlag)
	if err != nil {
		return err
	}
	closeServiceLogs, err := redirectServiceLogs(root)
	if err != nil {
		return err
	}
	defer closeServiceLogs()
	log.SetOutput(os.Stdout)
	log.AddHook(errorOnlyHook{})
	configPath := *configFlag
	if configPath == "" {
		configPath = filepath.Join(root, "runtime", "config.json")
	}
	gateway, err := loadGatewayConfig(configPath)
	if err != nil {
		return err
	}
	wopalKey, err := loadWopalAPIKey()
	if err != nil {
		return err
	}
	configureWopalCompactionKey(wopalKey)
	cliConfig, cliConfigPath, err := buildCLIProxyConfig(root, gateway, wopalKey)
	if err != nil {
		return err
	}
	logger, err := newActivityLogger(root)
	if err != nil {
		return fmt.Errorf("initialize activity logger: %w", err)
	}
	logger.write(map[string]any{
		"event": "gateway_starting", "host": gateway.Listen.Host, "port": gateway.Listen.Port,
		"wopal_models": len(gateway.WopalAI.Models), "ollama_enabled": false, "relay_port": nil,
	})
	quotaGuard := newOfficialQuotaGuardWithState(logger, filepath.Join(root, "runtime", "official-quota-state.json"), time.Now())
	quotaGuard.notifier = newQuotaNotifier(logger, gateway.Notifications)
	logger.write(map[string]any{"event": "quota_notification_configured", "status": quotaGuard.notifier.status()})

	builder := cliproxy.NewBuilder().
		WithConfig(cliConfig).
		WithConfigPath(cliConfigPath).
		WithServerOptions(
			sdkapi.WithMiddleware(routingMiddlewareWithQuotaGuard(logger, quotaGuard)),
			sdkapi.WithRouterConfigurator(func(engine *gin.Engine, _ *handlers.BaseAPIHandler, _ *sdkconfig.Config) {
				engine.GET("/_health", func(c *gin.Context) {
					c.JSON(200, gin.H{"ok": true, "mode": "single-process", "wopal_models": len(gateway.WopalAI.Models)})
				})
				engine.GET("/_status", func(c *gin.Context) {
					c.JSON(200, gin.H{
						"ok":             true,
						"listen":         fmt.Sprintf("%s:%d", gateway.Listen.Host, gateway.Listen.Port),
						"official":       "native-pass-through",
						"official_quota": quotaGuard.status(time.Now()),
						"notifications":  quotaGuard.notificationStatus(),
						"wopal":          "cliproxy-responses-to-chat",
						"ollama":         "disabled",
					})
				})
			}),
		)
	service, err := builder.Build()
	if err != nil {
		return fmt.Errorf("build embedded CLIProxyAPI service: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	startRequestBodyLogJanitor(ctx, filepath.Join(root, "runtime", "cliproxy-auth"), logger)
	logger.write(map[string]any{"event": "gateway_started", "architecture": "embedded-cliproxy", "port": gateway.Listen.Port})
	if err := service.Run(ctx); err != nil && ctx.Err() == nil {
		logger.write(map[string]any{"event": "gateway_failed", "error": err.Error()})
		return fmt.Errorf("run embedded CLIProxyAPI service: %w", err)
	}
	logger.write(map[string]any{"event": "gateway_stopped"})
	return nil
}

// redirectServiceLogs sends the embedded service's stdout and stderr to
// size-rotated files. The process keeps writing to an inherited descriptor, so
// each stream is captured through a pipe and copied into a rotating writer;
// handing the file descriptor straight to Dup2 would bypass rotation and keep
// appending to the renamed file forever.
func redirectServiceLogs(root string) (func(), error) {
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create logs directory: %w", err)
	}

	var cleanups []func()
	closeAll := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	stdout, err := captureStream(filepath.Join(logsDir, "service.log"), os.Stdout, nil)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("redirect stdout: %w", err)
	}
	cleanups = append(cleanups, stdout)

	stderr, err := captureStream(filepath.Join(logsDir, "service-error.log"), os.Stderr, newAbortHandlerFilterWriter)
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
func captureStream(path string, target *os.File, wrap func(io.Writer) io.Writer) (func(), error) {
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

	out := newRotatingWriter(path)
	var sink io.Writer = out
	var filtered *abortHandlerFilterWriter
	if wrap != nil {
		sink = wrap(out)
		filtered, _ = sink.(*abortHandlerFilterWriter)
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
			filtered.flush()
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

func resolveProjectRoot(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	if executable, err := os.Executable(); err == nil {
		candidate := filepath.Clean(filepath.Join(filepath.Dir(executable), "..", ".."))
		if _, err := os.Stat(filepath.Join(candidate, "package.json")); err == nil {
			return candidate, nil
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "package.json")); err != nil {
		return "", fmt.Errorf("cannot infer project root; pass --project-root")
	}
	return cwd, nil
}
