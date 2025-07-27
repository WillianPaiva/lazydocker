//go:build !windows

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	defaultDockerHost = "unix:///var/run/docker.sock"
	
	// Podman socket paths
	// System socket (requires root)
	defaultPodmanSystemSocket = "unix:///run/podman/podman.sock"
	
	// User socket (rootless mode)
	defaultPodmanUserSocketFormat = "unix:///run/user/%d/podman/podman.sock"
)

// GetDefaultPodmanHost determines the appropriate socket path for Podman
func GetDefaultPodmanHost() string {
	// Check for macOS-specific Podman socket paths
	homeDir, err := os.UserHomeDir()
	if err == nil {
		// On macOS, Podman typically uses ~/.local/share/containers/podman/machine/podman.sock
		macOSPodmanPaths := []string{
			filepath.Join(homeDir, ".local/share/containers/podman/machine/podman.sock"),
			filepath.Join(homeDir, ".local/share/containers/podman/machine/qemu/podman.sock"),
			filepath.Join(homeDir, ".local/share/containers/podman/machine/socket/podman.sock"),
			filepath.Join(homeDir, ".podman/machine/qemu/podman.sock"),
			filepath.Join(homeDir, ".podman/machine/podman.sock"),
		}

		for _, path := range macOSPodmanPaths {
			if _, err := os.Stat(path); err == nil {
				fmt.Printf("Found macOS Podman socket at %s\n", path)
				return "unix://" + path
			}
		}
		
		// Check if PODMAN_SOCKET environment variable is set
		if podmanSocket := os.Getenv("PODMAN_SOCKET"); podmanSocket != "" {
			if _, err := os.Stat(podmanSocket); err == nil {
				fmt.Printf("Found Podman socket from PODMAN_SOCKET env at %s\n", podmanSocket)
				return "unix://" + podmanSocket
			}
		}

		// On macOS, if the machine is running, we need to get the connection details
		if runtime.GOOS == "darwin" {
			// Just use the DOCKER_HOST we set up in the NewDockerCommand function
			if dockerHost := os.Getenv("DOCKER_HOST"); dockerHost != "" {
				fmt.Println("Using DOCKER_HOST for Podman connection: " + dockerHost)
				return dockerHost
			}
			
			// Try to detect socket for macOS Podman machine
			cmd := exec.Command("podman", "machine", "inspect", "--format", "{{.ConnectionInfo.PodmanSocket.Path}}")
			socketOutput, socketErr := cmd.CombinedOutput()
			if socketErr == nil && len(socketOutput) > 0 {
				socketPath := strings.TrimSpace(string(socketOutput))
				if socketPath != "" {
					fmt.Printf("Found Podman socket via machine inspect: %s\n", socketPath)
					return "unix://" + socketPath
				}
			}
			
			// Fallback to default
			fmt.Println("No DOCKER_HOST found, using default TCP connection")
			return "tcp://localhost:8080"
		}
	}

	// First check if we have a system-wide podman socket (requires root)
	if _, err := os.Stat("/run/podman/podman.sock"); err == nil {
		fmt.Println("Found system-wide Podman socket at /run/podman/podman.sock")
		return defaultPodmanSystemSocket
	}
	
	// Check for user socket (rootless mode)
	uid := os.Getuid()
	userSocketPath := fmt.Sprintf("/run/user/%d/podman/podman.sock", uid)
	if _, err := os.Stat(userSocketPath); err == nil {
		fmt.Printf("Found user Podman socket at %s\n", userSocketPath)
		return fmt.Sprintf(defaultPodmanUserSocketFormat, uid)
	}
	
	// XDG_RUNTIME_DIR environment variable is usually set to /run/user/$UID
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		podmanSocketPath := fmt.Sprintf("%s/podman/podman.sock", runtimeDir)
		if _, err := os.Stat(podmanSocketPath); err == nil {
			fmt.Printf("Found Podman socket via XDG_RUNTIME_DIR at %s\n", podmanSocketPath)
			return "unix://" + podmanSocketPath
		}
	}
	
	// Fall back to docker socket as a last resort
	fmt.Println("No Podman socket found, falling back to Docker socket")
	return defaultDockerHost
}
