package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	"wopal-codex-gateway/internal/activity"
	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/keychain"
)

var (
	runtimeGetWopalAPIKey   = keychain.GetWopalAPIKey
	runtimeLoadProviderKeys = loadRuntimeProviderKeys
	startGatewayBackendHook = startGatewayBackendWithLogger
)

type runtimeConfigCheckpoint struct {
	path    string
	data    []byte
	existed bool
}

func captureRuntimeConfigCheckpoint(root string) (*runtimeConfigCheckpoint, error) {
	path := config.CLIProxyRuntimeConfigPathFor(root)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &runtimeConfigCheckpoint{path: path}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read previous CLIProxy runtime config: %w", err)
	}
	return &runtimeConfigCheckpoint{path: path, data: data, existed: true}, nil
}

func (c *runtimeConfigCheckpoint) Restore() error {
	if c == nil || c.path == "" {
		return nil
	}
	if !c.existed {
		if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove failed candidate CLIProxy runtime config: %w", err)
		}
		return nil
	}
	if err := config.WriteFileAtomic(c.path, c.data, 0o600); err != nil {
		return fmt.Errorf("restore previous CLIProxy runtime config: %w", err)
	}
	return nil
}

func startGatewayBackend(root, gatewayConfigPath, instance string) (*runtimeBackend, *gatewaySnapshot, error) {
	return startGatewayBackendWithLogger(root, gatewayConfigPath, instance, nil)
}

func startGatewayBackendWithLogger(root, gatewayConfigPath, instance string, sharedLogger *activity.Logger) (*runtimeBackend, *gatewaySnapshot, error) {
	snapshot, err := loadGatewaySnapshot(root, gatewayConfigPath)
	if err != nil {
		return nil, nil, err
	}
	wopalKey, err := runtimeGetWopalAPIKey()
	if err != nil {
		return nil, nil, err
	}
	providerKeys, err := runtimeLoadProviderKeys(snapshot.gateway)
	if err != nil {
		return nil, nil, err
	}

	port, err := freeLoopbackPort()
	if err != nil {
		return nil, nil, err
	}
	internal := *snapshot.gateway
	internal.Listen.Host = "127.0.0.1"
	internal.Listen.Port = port
	service, logger, err := buildGatewayServiceWithRuntimeLogger(root, &internal, wopalKey, providerKeys, instance, snapshot.routes.Resolve, true, sharedLogger)
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- service.Run(ctx) }()
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	if err := waitBackendHealth(target, errCh); err != nil {
		cancel()
		shutdownService(service)
		if sharedLogger == nil {
			logger.Close()
		}
		return nil, nil, err
	}

	reverse := httputil.NewSingleHostReverseProxy(target)
	director := reverse.Director
	reverse.Director = func(req *http.Request) {
		director(req)
		// Keep loopback requests generated inside a websocket bound to this
		// snapshot instead of sending them through the stable front door where a
		// newer snapshot may already be active.
		req.Host = target.Host
	}
	backend := newRuntimeBackend(reverse, func() {
		cancel()
		shutdownService(service)
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
		}
		if sharedLogger == nil {
			logger.Close()
		}
	})
	backend.logger = logger
	backend.target = target
	return backend, snapshot, nil
}

func reloadGatewayBackend(root, gatewayConfigPath, instance string, listen config.ListenConfig, sharedLogger *activity.Logger) (*runtimeBackend, error) {
	checkpoint, err := captureRuntimeConfigCheckpoint(root)
	if err != nil {
		return nil, err
	}
	candidate, snapshot, err := startGatewayBackendHook(root, gatewayConfigPath, instance, sharedLogger)
	if err != nil {
		if restoreErr := checkpoint.Restore(); restoreErr != nil {
			return nil, fmt.Errorf("%v; restore runtime config: %w", err, restoreErr)
		}
		return nil, err
	}
	if !sameListen(listen, snapshot.gateway.Listen) {
		candidate.stopNow()
		reloadErr := fmt.Errorf("listen address changed; use service restart")
		if restoreErr := checkpoint.Restore(); restoreErr != nil {
			return nil, fmt.Errorf("%v; restore runtime config: %w", reloadErr, restoreErr)
		}
		return nil, reloadErr
	}
	return candidate, nil
}

func freeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate internal gateway port: %w", err)
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || addr.Port <= 0 {
		return 0, fmt.Errorf("allocate internal gateway port: invalid listener address %v", listener.Addr())
	}
	return addr.Port, nil
}

func waitBackendHealth(target *url.URL, errCh <-chan error) error {
	client := &http.Client{Timeout: 300 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-errCh:
			if err == nil {
				return fmt.Errorf("internal gateway stopped before becoming healthy")
			}
			return fmt.Errorf("start internal gateway: %w", err)
		default:
		}
		response, err := client.Get(target.String() + "/_health")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("internal gateway did not become healthy at %s", target)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func shutdownService(service *cliproxy.Service) {
	if service == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = service.Shutdown(ctx)
}

func logBackend(backend *runtimeBackend, fields map[string]any) {
	if backend != nil && backend.logger != nil {
		backend.logger.Write(fields)
	}
}

func sameListen(left, right config.ListenConfig) bool {
	return left.Host == right.Host && left.Port == right.Port
}
