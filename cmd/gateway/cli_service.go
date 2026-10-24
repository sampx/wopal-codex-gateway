package main

import (
	"fmt"
	"os"
	"strings"

	"wopal-codex-gateway/internal/config"
	"wopal-codex-gateway/internal/launchd"
)

// runServiceCommand implements the LaunchAgent lifecycle. The launchd calls and
// the health probe are package-level seams, so the whole flow is testable
// without touching a real user job.
func runServiceCommand(subcommand string, args []string) error {
	switch subcommand {
	case "install":
		return serviceInstall()
	case "uninstall":
		return serviceUninstall()
	case "start":
		return serviceStart()
	case "stop":
		return serviceStop()
	case "restart":
		return serviceRestart()
	case "status":
		return serviceStatus()
	}
	return fmt.Errorf("unknown service subcommand %q; usage: %s service <install|start|stop|restart|status|uninstall>", subcommand, binaryName)
}

func serviceInstall() error {
	// Fail before touching launchd when the install cannot possibly run.
	gateway, err := config.LoadInstallConfig()
	if err != nil {
		return err
	}
	if err := launchd.RequireGatewayBinary(); err != nil {
		return err
	}
	// InstallLaunchAgent already bootstraps, kickstarts and requires the
	// launch-scoped instance to answer /_health, so success here means the
	// daemon this install started is up.
	if err := launchd.InstallLaunchAgent(gateway.Listen.Host, gateway.Listen.Port); err != nil {
		return err
	}
	host, port := gateway.Listen.Host, gateway.Listen.Port
	fmt.Println("LaunchAgent installed and proxy started.")
	fmt.Printf("Proxy service is healthy: %s\n", launchd.HealthURL(host, port))
	return nil
}

func serviceUninstall() error {
	if err := launchd.UninstallLaunchAgent(); err != nil {
		return err
	}
	fmt.Println("LaunchAgent unloaded and removed.")
	return nil
}

func serviceStart() error {
	if err := requireInstalledLaunchAgent(); err != nil {
		return err
	}
	host, port, err := serviceListenAddress()
	if err != nil {
		return err
	}
	if serviceLoaded() {
		if err := launchd.RunLaunchctl("kickstart", "-k", launchd.LaunchAgentTarget()); err != nil {
			return fmt.Errorf("launchctl kickstart %s failed: %w", launchd.LaunchAgentTarget(), err)
		}
	} else if err := launchd.RunLaunchctl("bootstrap", launchd.LaunchdDomain(), config.LaunchAgentPath()); err != nil {
		return fmt.Errorf("launchctl bootstrap %s failed: %w", config.LaunchAgentPath(), err)
	}
	return reportServiceRunning(host, port)
}

func serviceRestart() error {
	if err := requireInstalledLaunchAgent(); err != nil {
		return err
	}
	host, port, err := serviceListenAddress()
	if err != nil {
		return err
	}
	if err := launchd.RunLaunchctl("kickstart", "-k", launchd.LaunchAgentTarget()); err != nil {
		return fmt.Errorf("launchctl kickstart %s failed: %w", launchd.LaunchAgentTarget(), err)
	}
	return reportServiceRunning(host, port)
}

func serviceStop() error {
	if serviceLoaded() {
		if err := launchd.RunLaunchctl("bootout", launchd.LaunchAgentTarget()); err != nil {
			return fmt.Errorf("launchctl bootout %s failed: %w", launchd.LaunchAgentTarget(), err)
		}
	}
	fmt.Println("Proxy stopped.")
	return nil
}

func serviceStatus() error {
	// Read the config first: an unsupported or unparsable config is the more
	// useful error, and it keeps every config-reading subcommand failing the
	// same way before it touches launchd or the network.
	host, port, err := serviceListenAddress()
	if err != nil {
		return err
	}
	installed := launchAgentPlistExists()
	loaded := serviceLoaded()
	healthy := launchd.ProbeHealth(launchd.HealthURL(host, port), launchd.StatusHealthTimeout) == nil

	state := fmt.Sprintf("installed=%s loaded=%s healthy=%s", yesNo(installed), yesNo(loaded), yesNo(healthy))
	fmt.Println(state)
	if installed && loaded && healthy {
		return nil
	}
	return fmt.Errorf("proxy service is not ready (%s); check %s and %s", state, config.ServiceLogPath(), config.ServiceErrorLogPath())
}

func reportServiceRunning(host string, port int) error {
	if err := launchd.WaitForHealth(host, port); err != nil {
		return fmt.Errorf("proxy health check failed: %w; check %s and %s", err, config.ServiceLogPath(), config.ServiceErrorLogPath())
	}
	fmt.Printf("Proxy running: %s\n", launchd.HealthURL(host, port))
	return nil
}

// serviceListenAddress reads the configured listen address. An unreadable or
// unsupported config is reported instead of being replaced by the default: a
// silent fallback would probe a port this instance does not own and would report
// success for a configuration the daemon itself refuses to serve.
func serviceListenAddress() (string, int, error) {
	gatewayConfig, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		return "", 0, err
	}
	host := strings.TrimSpace(gatewayConfig.Listen.Host)
	if host == "" {
		host = config.DefaultListenHost
	}
	port := gatewayConfig.Listen.Port
	if port == 0 {
		port = config.DefaultListenPort
	}
	return host, port, nil
}

func serviceLoaded() bool {
	return launchd.RunLaunchctl("print", launchd.LaunchAgentTarget()) == nil
}

func launchAgentPlistExists() bool {
	_, err := os.Stat(config.LaunchAgentPath())
	return err == nil
}

func requireInstalledLaunchAgent() error {
	if launchAgentPlistExists() {
		return nil
	}
	return fmt.Errorf("proxy service is not installed; run '%s service install' first", binaryName)
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
