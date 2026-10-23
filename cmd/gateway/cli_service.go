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
	if _, err := config.LoadInstallConfig(); err != nil {
		return err
	}
	if err := launchd.RequireGatewayBinary(); err != nil {
		return err
	}
	if err := launchd.InstallLaunchAgent(); err != nil {
		return err
	}
	host, port := serviceListenAddress()
	if err := launchd.WaitForHealth(host, port); err != nil {
		return fmt.Errorf("proxy health check failed: %w; check %s and %s", err, config.ServiceLogPath(), config.ServiceErrorLogPath())
	}
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
	if serviceLoaded() {
		if err := launchd.RunLaunchctl("kickstart", "-k", launchd.LaunchAgentTarget()); err != nil {
			return fmt.Errorf("launchctl kickstart %s failed: %w", launchd.LaunchAgentTarget(), err)
		}
	} else if err := launchd.RunLaunchctl("bootstrap", launchd.LaunchdDomain(), config.LaunchAgentPath()); err != nil {
		return fmt.Errorf("launchctl bootstrap %s failed: %w", config.LaunchAgentPath(), err)
	}
	return reportServiceRunning()
}

func serviceRestart() error {
	if err := requireInstalledLaunchAgent(); err != nil {
		return err
	}
	if err := launchd.RunLaunchctl("kickstart", "-k", launchd.LaunchAgentTarget()); err != nil {
		return fmt.Errorf("launchctl kickstart %s failed: %w", launchd.LaunchAgentTarget(), err)
	}
	return reportServiceRunning()
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
	installed := launchAgentPlistExists()
	loaded := serviceLoaded()
	host, port := serviceListenAddress()
	healthy := launchd.ProbeHealth(launchd.HealthURL(host, port), launchd.StatusHealthTimeout) == nil

	state := fmt.Sprintf("installed=%s loaded=%s healthy=%s", yesNo(installed), yesNo(loaded), yesNo(healthy))
	fmt.Println(state)
	if installed && loaded && healthy {
		return nil
	}
	return fmt.Errorf("proxy service is not ready (%s); check %s and %s", state, config.ServiceLogPath(), config.ServiceErrorLogPath())
}

func reportServiceRunning() error {
	host, port := serviceListenAddress()
	if err := launchd.WaitForHealth(host, port); err != nil {
		return fmt.Errorf("proxy health check failed: %w; check %s and %s", err, config.ServiceLogPath(), config.ServiceErrorLogPath())
	}
	fmt.Printf("Proxy running: %s\n", launchd.HealthURL(host, port))
	return nil
}

// serviceListenAddress reads the configured listen address, falling back to the
// documented default when the config is unreadable.
func serviceListenAddress() (string, int) {
	gatewayConfig, err := config.LoadGatewayConfig(config.ConfigPath())
	if err != nil {
		return config.DefaultListenHost, config.DefaultListenPort
	}
	host := strings.TrimSpace(gatewayConfig.Listen.Host)
	if host == "" {
		host = config.DefaultListenHost
	}
	port := gatewayConfig.Listen.Port
	if port == 0 {
		port = config.DefaultListenPort
	}
	return host, port
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
