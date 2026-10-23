package main

import (
	"fmt"
	"os"
	"strings"
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
	if _, err := loadInstallConfig(); err != nil {
		return err
	}
	if err := requireGatewayBinary(); err != nil {
		return err
	}
	if err := installLaunchAgent(); err != nil {
		return err
	}
	host, port := serviceListenAddress()
	if err := waitForHealth(host, port); err != nil {
		return fmt.Errorf("proxy health check failed: %w; check %s and %s", err, serviceLogPath(), serviceErrorLogPath())
	}
	fmt.Println("LaunchAgent installed and proxy started.")
	fmt.Printf("Proxy service is healthy: %s\n", healthURL(host, port))
	return nil
}

func serviceUninstall() error {
	if err := uninstallLaunchAgent(); err != nil {
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
		if err := runLaunchctl("kickstart", "-k", launchAgentTarget()); err != nil {
			return fmt.Errorf("launchctl kickstart %s failed: %w", launchAgentTarget(), err)
		}
	} else if err := runLaunchctl("bootstrap", launchdDomain(), launchAgentPath()); err != nil {
		return fmt.Errorf("launchctl bootstrap %s failed: %w", launchAgentPath(), err)
	}
	return reportServiceRunning()
}

func serviceRestart() error {
	if err := requireInstalledLaunchAgent(); err != nil {
		return err
	}
	if err := runLaunchctl("kickstart", "-k", launchAgentTarget()); err != nil {
		return fmt.Errorf("launchctl kickstart %s failed: %w", launchAgentTarget(), err)
	}
	return reportServiceRunning()
}

func serviceStop() error {
	if serviceLoaded() {
		if err := runLaunchctl("bootout", launchAgentTarget()); err != nil {
			return fmt.Errorf("launchctl bootout %s failed: %w", launchAgentTarget(), err)
		}
	}
	fmt.Println("Proxy stopped.")
	return nil
}

func serviceStatus() error {
	installed := launchAgentPlistExists()
	loaded := serviceLoaded()
	host, port := serviceListenAddress()
	healthy := probeHealth(healthURL(host, port), statusHealthTimeout) == nil

	state := fmt.Sprintf("installed=%s loaded=%s healthy=%s", yesNo(installed), yesNo(loaded), yesNo(healthy))
	fmt.Println(state)
	if installed && loaded && healthy {
		return nil
	}
	return fmt.Errorf("proxy service is not ready (%s); check %s and %s", state, serviceLogPath(), serviceErrorLogPath())
}

func reportServiceRunning() error {
	host, port := serviceListenAddress()
	if err := waitForHealth(host, port); err != nil {
		return fmt.Errorf("proxy health check failed: %w; check %s and %s", err, serviceLogPath(), serviceErrorLogPath())
	}
	fmt.Printf("Proxy running: %s\n", healthURL(host, port))
	return nil
}

// serviceListenAddress reads the configured listen address, falling back to the
// documented default when the config is unreadable.
func serviceListenAddress() (string, int) {
	config, err := loadGatewayConfig(configPath())
	if err != nil {
		return defaultListenHost, defaultListenPort
	}
	host := strings.TrimSpace(config.Listen.Host)
	if host == "" {
		host = defaultListenHost
	}
	port := config.Listen.Port
	if port == 0 {
		port = defaultListenPort
	}
	return host, port
}

func serviceLoaded() bool {
	return runLaunchctl("print", launchAgentTarget()) == nil
}

func launchAgentPlistExists() bool {
	_, err := os.Stat(launchAgentPath())
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
