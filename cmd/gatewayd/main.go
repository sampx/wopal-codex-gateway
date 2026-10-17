package main

import (
	"context"
	"flag"
	"fmt"
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
	if err := redirectServiceLogs(root); err != nil {
		return err
	}
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
	logger.write(map[string]any{"event": "gateway_started", "architecture": "embedded-cliproxy", "port": gateway.Listen.Port})
	if err := service.Run(ctx); err != nil && ctx.Err() == nil {
		logger.write(map[string]any{"event": "gateway_failed", "error": err.Error()})
		return fmt.Errorf("run embedded CLIProxyAPI service: %w", err)
	}
	logger.write(map[string]any{"event": "gateway_stopped"})
	return nil
}

func redirectServiceLogs(root string) error {
	logsDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		return fmt.Errorf("create logs directory: %w", err)
	}
	stdout, err := os.OpenFile(filepath.Join(logsDir, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open service log: %w", err)
	}
	stderr, err := os.OpenFile(filepath.Join(logsDir, "service-error.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = stdout.Close()
		return fmt.Errorf("open service error log: %w", err)
	}
	if err := syscall.Dup2(int(stdout.Fd()), int(os.Stdout.Fd())); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return fmt.Errorf("redirect stdout: %w", err)
	}
	if err := syscall.Dup2(int(stderr.Fd()), int(os.Stderr.Fd())); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return fmt.Errorf("redirect stderr: %w", err)
	}
	_ = stdout.Close()
	_ = stderr.Close()
	return nil
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
