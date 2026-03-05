package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"

	"adbtest/internal/adb"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

const (
	labelManaged = "adbtest.managed"
	labelDevice  = "adbtest.device"
	labelPort    = "adbtest.port"
	labelRole    = "adbtest.role"

	roleAppium = "appium"
	roleTests  = "tests"

	// adbNetwork is a dedicated bridge network shared by appium + test containers
	// so they can reach each other by container name without going through the host.
	adbNetwork = "adbtest"
)

// Config holds the manager configuration.
type Config struct {
	AppiumImage string
	TestImage   string // image to use for test containers ("" = disabled)
	BasePort    int
	ADBHost     string // hostname of ADB server, reachable from Appium containers
	ADBPort     int
}

// deviceContainers tracks the pair of containers managed for one device.
type deviceContainers struct {
	AppiumID   string
	AppiumPort int
	AppiumName string

	TestID     string
	TestStatus string // "running" | "exited" | "created" | ""
}

// Manager handles the lifecycle of Appium (and optionally test) Docker containers.
type Manager struct {
	cli    *client.Client
	config Config
}

// NewManager creates a new Manager.
func NewManager(cli *client.Client, cfg Config) *Manager {
	return &Manager{cli: cli, config: cfg}
}

// Reconcile brings the set of running containers in sync with connected ADB devices.
func (m *Manager) Reconcile(ctx context.Context, devices []adb.Device) error {
	if err := m.ensureNetwork(ctx); err != nil {
		return fmt.Errorf("ensure network: %w", err)
	}

	existing, err := m.listManaged(ctx)
	if err != nil {
		return fmt.Errorf("list managed containers: %w", err)
	}

	readyDevices := make(map[string]adb.Device)
	for _, d := range devices {
		if d.IsReady() {
			readyDevices[d.Serial] = d
		}
	}

	// Remove containers for disconnected devices.
	for serial, dc := range existing {
		if _, connected := readyDevices[serial]; !connected {
			log.Printf("[remove] device %s disconnected", serial)
			m.removeDevice(ctx, dc)
		}
	}

	// Create containers for new devices.
	for serial, dev := range readyDevices {
		dc, running := existing[serial]

		// --- Appium container ---
		if !running || dc.AppiumID == "" {
			port := m.nextPort(existing)
			log.Printf("[create] appium for device %s on host port %d", serial, port)

			newDC, err := m.createAppium(ctx, dev, port)
			if err != nil {
				log.Printf("[create] appium for %s: %v", serial, err)
				continue
			}
			dc = *newDC
			existing[serial] = dc
		} else {
			log.Printf("[skip] appium already running for %s (port %d)", serial, dc.AppiumPort)
		}

		// --- Test container (optional) ---
		if m.config.TestImage == "" {
			continue
		}

		if dc.TestID != "" && dc.TestStatus == "running" {
			log.Printf("[skip] tests already running for %s", serial)
			continue
		}

		// Remove a stopped test container so we can recreate it.
		if dc.TestID != "" {
			log.Printf("[cleanup] removing stopped test container for %s", serial)
			_ = m.cli.ContainerRemove(ctx, dc.TestID, container.RemoveOptions{Force: true})
		}

		log.Printf("[create] test container for device %s → appium host.docker.internal:%d", serial, dc.AppiumPort)
		if err := m.createTest(ctx, dev, dc.AppiumPort); err != nil {
			log.Printf("[create] test container for %s: %v", serial, err)
		}
	}

	return nil
}

// PullImage pulls the given image, streaming progress to stdout.
func (m *Manager) PullImage(ctx context.Context, img string) error {
	log.Printf("Pulling image %s ...", img)
	rc, err := m.cli.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("image pull %s: %w", img, err)
	}
	defer rc.Close()
	_, err = io.Copy(os.Stdout, rc)
	return err
}

// BuildTestImage builds the test Docker image from the given context directory.
func (m *Manager) BuildTestImage(ctx context.Context, contextDir, tag string) error {
	log.Printf("Building test image %s from %s ...", tag, contextDir)
	// Use docker CLI for simplicity (avoids streaming tar complexity in SDK).
	// A full SDK build is possible but verbose for this use-case.
	return runCmd(ctx, "docker", "build", "-t", tag, contextDir)
}

// ── internal helpers ──────────────────────────────────────────────────────────

func (m *Manager) ensureNetwork(ctx context.Context) error {
	f := filters.NewArgs(filters.Arg("name", adbNetwork))
	nets, err := m.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		return err
	}
	for _, n := range nets {
		if n.Name == adbNetwork {
			return nil // already exists
		}
	}
	log.Printf("Creating Docker network %q", adbNetwork)
	_, err = m.cli.NetworkCreate(ctx, adbNetwork, network.CreateOptions{Driver: "bridge"})
	return err
}

// listManaged returns a map[serial]deviceContainers for all managed containers.
func (m *Manager) listManaged(ctx context.Context) (map[string]deviceContainers, error) {
	f := filters.NewArgs(filters.Arg("label", labelManaged+"=true"))
	list, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}

	result := make(map[string]deviceContainers)
	for _, c := range list {
		serial := c.Labels[labelDevice]
		dc := result[serial]

		switch c.Labels[labelRole] {
		case roleAppium:
			port, _ := strconv.Atoi(c.Labels[labelPort])
			name := ""
			if len(c.Names) > 0 {
				name = c.Names[0][1:] // strip leading "/"
			}
			dc.AppiumID = c.ID
			dc.AppiumPort = port
			dc.AppiumName = name
		case roleTests:
			dc.TestID = c.ID
			dc.TestStatus = c.State // "running", "exited", etc.
		}
		result[serial] = dc
	}
	return result, nil
}

// nextPort returns the lowest port >= BasePort not already used.
func (m *Manager) nextPort(existing map[string]deviceContainers) int {
	used := make(map[int]bool, len(existing))
	for _, dc := range existing {
		if dc.AppiumPort > 0 {
			used[dc.AppiumPort] = true
		}
	}
	port := m.config.BasePort
	for used[port] {
		port++
	}
	return port
}

// createAppium starts an Appium container for the given device.
//
// The container runs with --network=host so that `adb forward` port bindings
// created by Appium on the host are accessible via localhost inside the
// container. Without host networking, Appium would forward device ports on the
// host but then fail to connect to them because "localhost" inside the
// container is the container itself, not the host.
//
// Host-network containers share the abstract Unix-socket namespace, so each
// Xvfb instance must use a unique display number (derived from hostPort) to
// avoid "Xvfb failed to start" when multiple devices are connected.
func (m *Manager) createAppium(ctx context.Context, dev adb.Device, hostPort int) (*deviceContainers, error) {
	name := "appium-" + sanitize(dev.Serial)

	// Free the port from any orphaned process before creating the container.
	// With --network=host, a previously force-removed container's Appium process
	// may still be alive and holding the port; kill it now so the bind succeeds.
	killPortHolder(hostPort)

	cfg := &container.Config{
		Image: m.config.AppiumImage,
		Env: []string{
			"ANDROID_SERIAL=" + dev.Serial,
			// With host networking, ADB server is on localhost.
			"ANDROID_ADB_SERVER_ADDRESS=localhost",
			fmt.Sprintf("ANDROID_ADB_SERVER_PORT=%d", m.config.ADBPort),
		},
		// Skip start.sh (which wraps appium in xvfb-run) — Xvfb is not needed
		// for Android/UiAutomator2, which talks to the device over ADB, not X11.
		// The image ENTRYPOINT is ["sh","-c"], so Cmd[0] is the shell script.
		Cmd: []string{fmt.Sprintf(
			`appium --log /var/log/appium.log --port %d --address 0.0.0.0`,
			hostPort,
		)},
		Labels: map[string]string{
			labelManaged: "true",
			labelDevice:  dev.Serial,
			labelRole:    roleAppium,
			labelPort:    strconv.Itoa(hostPort),
		},
	}

	hostCfg := &container.HostConfig{
		// Host networking: the container shares the host's network stack.
		// adb forward ports appear on 127.0.0.1 and Appium can reach them.
		NetworkMode: "host",
	}

	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return nil, fmt.Errorf("create: %w", err)
	}
	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("start: %w", err)
	}

	log.Printf("[appium] started %s (id=%s, host-network) → port %d", name, resp.ID[:12], hostPort)
	return &deviceContainers{
		AppiumID:   resp.ID,
		AppiumPort: hostPort,
		AppiumName: name,
	}, nil
}

// createTest starts a test container that connects to Appium running on the host network.
func (m *Manager) createTest(ctx context.Context, dev adb.Device, appiumPort int) error {
	name := "tests-" + sanitize(dev.Serial)

	cfg := &container.Config{
		Image: m.config.TestImage,
		Env: []string{
			"ANDROID_SERIAL=" + dev.Serial,
			// Appium runs on host network → reach it via host.docker.internal.
			"APPIUM_HOST=host.docker.internal",
			fmt.Sprintf("APPIUM_PORT=%d", appiumPort),
		},
		Labels: map[string]string{
			labelManaged: "true",
			labelDevice:  dev.Serial,
			labelRole:    roleTests,
		},
	}

	hostCfg := &container.HostConfig{
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
	}

	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("start: %w", err)
	}

	log.Printf("[tests] started %s (id=%s) → appium host.docker.internal:%d", name, resp.ID[:12], appiumPort)
	return nil
}

// removeDevice force-removes both containers for a device.
func (m *Manager) removeDevice(ctx context.Context, dc deviceContainers) {
	for _, id := range []string{dc.TestID, dc.AppiumID} {
		if id == "" {
			continue
		}
		if err := m.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
			log.Printf("[remove] container %s: %v", id[:12], err)
		}
	}
}

// sanitize replaces characters not safe for Docker container names with '-'.
func sanitize(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '.') {
			b[i] = '-'
		}
	}
	return string(b)
}
