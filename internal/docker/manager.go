package docker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"adbtest/internal/adb"
	"adbtest/internal/store"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	labelManaged   = "adbtest.managed"
	labelDevice    = "adbtest.device"
	labelPort      = "adbtest.port"
	labelRole      = "adbtest.role"
	labelModel     = "adbtest.model"
	labelStartedAt = "adbtest.started_at" // RFC3339 timestamp when test container was created
	labelBattery   = "adbtest.battery"    // battery % at test start (-1 = unknown)

	roleAppium = "appium"
	roleTests  = "tests"

	// adbNetwork is a dedicated bridge network shared by appium + test containers
	// so they can reach each other by container name without going through the host.
	adbNetwork = "adbtest"
)

// wdio spec-reporter output patterns.
// "4 passing (13.8s)"    → group 1 = count, group 3 = seconds
// "4 passing (1m 13.8s)" → group 1 = count, group 2 = minutes, group 3 = seconds
var (
	rePassing = regexp.MustCompile(`(\d+) passing(?:\s+\((?:(\d+)m\s*)?(\d+(?:\.\d+)?)s\))?`)
	reFailing = regexp.MustCompile(`(\d+) failing`)
	rePending = regexp.MustCompile(`(\d+) pending`)
)

// Config holds the manager configuration.
type Config struct {
	AppiumImage string
	TestImage   string // image to use for test containers ("" = disabled)
	BasePort    int
	ADBHost     string // hostname of ADB server, reachable from Appium containers
	ADBPort     int
	APKServeURL string // HTTP URL of the APK served by the built-in web server
	             //   e.g. "http://localhost:8080/apk/ApiDemos-debug.apk"
	             // Appium containers use --network=host so localhost resolves to the host.
}

// deviceContainers tracks the pair of containers managed for one device.
type deviceContainers struct {
	AppiumID   string
	AppiumPort int
	AppiumName string

	TestID        string
	TestStatus    string    // "running" | "exited" | "created" | ""
	TestStartedAt time.Time // when the test container was created
	DeviceModel   string    // ro.product.model, stored in container labels
	BatteryPct    int       // battery level at test start (-1 = unknown)
}

// testRunSummary carries the parsed wdio result for one device/run.
type testRunSummary struct {
	Serial     string
	Model      string
	StartedAt  time.Time // when the test container started
	FinishedAt time.Time
	Passing    int
	Failing    int
	Pending    int
	Found      bool    // false if container crashed before wdio produced any output
	TestSecs   float64 // wdio test execution time in seconds (from "N passing (Xs)")
	TestLog    []byte  // raw wdio output (saved to disk on failure)
	AppiumLog  []byte  // raw appium output (saved to disk on failure)
	Screenshot []byte  // PNG screenshot taken before reboot on failure
	BatteryPct int     // battery level at test start (-1 = unknown)
}

func (s testRunSummary) deviceLabel() string {
	if s.Model != "" {
		return fmt.Sprintf("%s (%s)", s.Serial, s.Model)
	}
	return s.Serial
}

// totalDuration returns the full container lifetime (setup + tests).
func (s testRunSummary) totalDuration() time.Duration {
	if s.StartedAt.IsZero() {
		return 0
	}
	return s.FinishedAt.Sub(s.StartedAt)
}

// setupDuration returns the time spent on setup (session init + APK install),
// computed as total - test execution.
func (s testRunSummary) setupDuration() time.Duration {
	total := s.totalDuration()
	test := time.Duration(s.TestSecs * float64(time.Second))
	if test > total {
		return 0
	}
	return total - test
}

// Manager handles the lifecycle of Appium (and optionally test) Docker containers.
type Manager struct {
	cli       *client.Client
	config    Config
	store     *store.Store
	NotifyFn  func()     // called after each run is saved; used for SSE push
	rebooting sync.Map   // serial → struct{}: device is mid-reboot, skip test creation
	reportMu  sync.Mutex // serialises writes to the daily report file
}

// NewManager creates a new Manager. st may be nil (SQLite disabled).
func NewManager(cli *client.Client, cfg Config, st *store.Store) *Manager {
	return &Manager{cli: cli, config: cfg, store: st}
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

		// Remove a stopped test container, report results, then reboot device.
		if dc.TestID != "" {
			summary := m.reportTestResult(ctx, serial, dc)
			log.Printf("[cleanup] removing stopped test container for %s", serial)
			_ = m.cli.ContainerRemove(ctx, dc.TestID, container.RemoveOptions{Force: true})
			m.rebooting.Store(serial, struct{}{})
			go m.rebootAndReport(summary)
			continue // test container will be recreated after device comes back
		}

		// Don't start tests while the device is rebooting.
		if _, isRebooting := m.rebooting.Load(serial); isRebooting {
			log.Printf("[skip] device %s is rebooting", serial)
			continue
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

		if model := c.Labels[labelModel]; model != "" {
			dc.DeviceModel = model
		}
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
			if ts := c.Labels[labelStartedAt]; ts != "" {
				t, _ := time.Parse(time.RFC3339, ts)
				dc.TestStartedAt = t
			}
			dc.BatteryPct = -1
			if bp := c.Labels[labelBattery]; bp != "" {
				n, _ := strconv.Atoi(bp)
				dc.BatteryPct = n
			}
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
			`appium --log /var/log/appium.log --port %d --address 0.0.0.0 --allow-insecure=adb_shell`,
			hostPort,
		)},
		Labels: map[string]string{
			labelManaged: "true",
			labelDevice:  dev.Serial,
			labelRole:    roleAppium,
			labelPort:    strconv.Itoa(hostPort),
			labelModel:   dev.Model,
		},
	}

	hostCfg := &container.HostConfig{
		// Host networking: the container shares the host's network stack.
		// adb forward ports appear on 127.0.0.1 and Appium can reach them.
		// It also means Appium can reach the built-in HTTP server on localhost
		// to download the APK (no volume mounts needed).
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
	now := time.Now().UTC()

	// Grant Appium overlay permissions — suppresses the "display over other
	// apps" dialog that can block tests. Best-effort: silently skipped if
	// Appium hasn't installed its helper packages yet (first-ever run).
	adb.GrantAppiumPermissions(dev.Serial)

	// Check battery level before starting tests — skip if below 30%.
	batt := -1
	if level, err := adb.BatteryLevel(dev.Serial); err != nil {
		log.Printf("[battery] %s: %v", dev.Serial, err)
	} else {
		batt = level
		log.Printf("[battery] %s: %d%%", dev.Serial, batt)
		if batt < 30 {
			log.Printf("[skip] %s battery too low (%d%% < 30%%), not starting tests", dev.Serial, batt)
			return fmt.Errorf("battery too low: %d%% (minimum 30%%)", batt)
		}
	}

	env := []string{
		"ANDROID_SERIAL=" + dev.Serial,
		// Appium runs on host network → reach it via host.docker.internal.
		"APPIUM_HOST=host.docker.internal",
		fmt.Sprintf("APPIUM_PORT=%d", appiumPort),
	}

	// Pass the APK URL so wdio.conf.js tells Appium where to fetch the APK.
	// Appium runs with --network=host and downloads the APK from the built-in
	// HTTP server (localhost) without any internet access.
	if m.config.APKServeURL != "" {
		env = append(env, "APIDEMOS_APK_URL="+m.config.APKServeURL)
	}

	cfg := &container.Config{
		Image: m.config.TestImage,
		Env:   env,
		Labels: map[string]string{
			labelManaged:   "true",
			labelDevice:    dev.Serial,
			labelRole:      roleTests,
			labelModel:     dev.Model,
			labelStartedAt: now.Format(time.RFC3339),
			labelBattery:   strconv.Itoa(batt),
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

// reportTestResult reads the logs of a finished test container, parses the
// wdio spec-reporter summary, logs it to stdout, and returns a testRunSummary
// for the file report (written later, after the device reboots).
func (m *Manager) reportTestResult(ctx context.Context, serial string, dc deviceContainers) testRunSummary {
	summary := testRunSummary{
		Serial:     serial,
		Model:      dc.DeviceModel,
		StartedAt:  dc.TestStartedAt,
		FinishedAt: time.Now(),
		BatteryPct: dc.BatteryPct,
	}

	rc, err := m.cli.ContainerLogs(ctx, dc.TestID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		log.Printf("[report] %s: could not read logs: %v", serial, err)
		return summary
	}
	defer rc.Close()

	// Docker log stream is multiplexed (stdout/stderr); stdcopy demuxes it.
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, rc); err != nil {
		_, _ = io.Copy(&buf, rc)
	}
	summary.TestLog = buf.Bytes()

	// Capture Appium container logs (last 2000 lines to keep file size reasonable).
	if dc.AppiumID != "" {
		arc, err := m.cli.ContainerLogs(ctx, dc.AppiumID, container.LogsOptions{
			ShowStdout: true,
			ShowStderr: true,
			Tail:       "2000",
		})
		if err == nil {
			var abuf bytes.Buffer
			if _, err := stdcopy.StdCopy(&abuf, &abuf, arc); err != nil {
				_, _ = io.Copy(&abuf, arc)
			}
			arc.Close()
			summary.AppiumLog = abuf.Bytes()
		}
	}

	scanner := bufio.NewScanner(&buf)
	for scanner.Scan() {
		line := scanner.Text()
		if ms := rePassing.FindStringSubmatch(line); ms != nil {
			summary.Passing, _ = strconv.Atoi(ms[1])
			summary.Found = true
			// ms[2] = minutes (optional), ms[3] = seconds
			if ms[3] != "" {
				secs, _ := strconv.ParseFloat(ms[3], 64)
				if ms[2] != "" {
					mins, _ := strconv.Atoi(ms[2])
					secs += float64(mins) * 60
				}
				summary.TestSecs = secs
			}
		}
		if ms := reFailing.FindStringSubmatch(line); ms != nil {
			summary.Failing, _ = strconv.Atoi(ms[1])
			summary.Found = true
		}
		if ms := rePending.FindStringSubmatch(line); ms != nil {
			summary.Pending, _ = strconv.Atoi(ms[1])
		}
	}

	// Take a screenshot before the device is rebooted (only on failure/crash).
	if !summary.Found || summary.Failing > 0 {
		sCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(sCtx, "adb", "-s", serial, "exec-out", "screencap", "-p")
		if png, err := cmd.Output(); err == nil && len(png) > 0 {
			summary.Screenshot = png
			log.Printf("[screenshot] captured for %s (%d bytes)", serial, len(png))
		} else if err != nil {
			log.Printf("[screenshot] %s: %v", serial, err)
		}
	}

	sep := strings.Repeat("─", 52)
	log.Printf("[report] %s", sep)
	if !summary.Found {
		log.Printf("[report] %s — no results (container crashed?)", summary.deviceLabel())
	} else {
		verdict := "PASS"
		if summary.Failing > 0 {
			verdict = "FAIL"
		}
		log.Printf("[report] %s  %s", verdict, summary.deviceLabel())
		log.Printf("[report]        passing: %d | failing: %d | pending: %d",
			summary.Passing, summary.Failing, summary.Pending)
		if total := summary.totalDuration(); total > 0 {
			log.Printf("[report]        total: %s  setup: %s  tests: %.1fs",
				total.Round(time.Second),
				summary.setupDuration().Round(time.Second),
				summary.TestSecs)
		}
	}
	log.Printf("[report] %s", sep)
	return summary
}

// rebootAndReport reboots the device, waits for it to come back, then writes
// the full report entry (test results + boot time) to the daily report file.
func (m *Manager) rebootAndReport(summary testRunSummary) {
	defer m.rebooting.Delete(summary.Serial)

	log.Printf("[reboot] rebooting %s...", summary.deviceLabel())
	rebootAt := time.Now()

	if err := adb.Reboot(summary.Serial); err != nil {
		log.Printf("[reboot] %s: %v", summary.Serial, err)
		m.writeFileReport(summary, 0, rebootAt, false)
		return
	}

	bootDuration, err := adb.WaitForReady(summary.Serial, 5*time.Minute)
	if err != nil {
		log.Printf("[reboot] %s: %v", summary.Serial, err)
		m.writeFileReport(summary, bootDuration, rebootAt, false)
		return
	}

	log.Printf("[reboot] %s ready after %s", summary.deviceLabel(), bootDuration.Round(time.Second))
	// Grant Appium overlay permission so the next test run is not blocked
	// by the "display over other apps" system dialog.
	adb.GrantAppiumPermissions(summary.Serial)
	m.writeFileReport(summary, bootDuration, rebootAt, true)
}

// fmtDuration formats a duration as "1m 5s" or "45s".
func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm %ds", m, s)
}

// writeFileReport appends a structured entry to reports/YYYY-MM-DD.log.
func (m *Manager) writeFileReport(summary testRunSummary, bootDuration time.Duration, rebootAt time.Time, bootOK bool) {
	if err := os.MkdirAll("reports", 0o755); err != nil {
		log.Printf("[report] mkdir reports: %v", err)
		return
	}

	filename := fmt.Sprintf("reports/%s.log", summary.FinishedAt.Format("2006-01-02"))

	m.reportMu.Lock()
	defer m.reportMu.Unlock()

	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("[report] open %s: %v", filename, err)
		return
	}
	defer f.Close()

	sep := strings.Repeat("─", 60)
	fmt.Fprintln(f, sep)
	fmt.Fprintf(f, "Time:    %s\n", summary.FinishedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(f, "Device:  %s\n", summary.deviceLabel())
	if summary.BatteryPct >= 0 {
		fmt.Fprintf(f, "Battery: %d%%\n", summary.BatteryPct)
	}

	if !summary.Found {
		fmt.Fprintln(f, "Tests:   no results (container crashed before tests ran)")
	} else {
		verdict := "PASS"
		if summary.Failing > 0 {
			verdict = "FAIL"
		}
		fmt.Fprintf(f, "Tests:   %s  |  passing: %d  failing: %d  pending: %d\n",
			verdict, summary.Passing, summary.Failing, summary.Pending)
	}

	if total := summary.totalDuration(); total > 0 {
		fmt.Fprintf(f, "Timing:  total: %s  |  setup: %s  |  tests: %.1fs\n",
			fmtDuration(total),
			fmtDuration(summary.setupDuration()),
			summary.TestSecs)
	}

	if bootOK {
		readyAt := rebootAt.Add(bootDuration)
		fmt.Fprintf(f, "Reboot:  %s  (started: %s, ready: %s)\n",
			bootDuration.Round(time.Second),
			rebootAt.Format("15:04:05"),
			readyAt.Format("15:04:05"))
	} else {
		fmt.Fprintf(f, "Reboot:  FAILED or timed out (elapsed: %s)\n",
			bootDuration.Round(time.Second))
	}

	fmt.Fprintln(f, sep)
	fmt.Fprintln(f, "")
	log.Printf("[report] written to %s", filename)

	// Also persist to SQLite if a store is configured.
	if m.store != nil {
		run := store.Run{
			Serial:       summary.Serial,
			Model:        summary.Model,
			FinishedAt:   summary.FinishedAt,
			Passing:      summary.Passing,
			Failing:      summary.Failing,
			Pending:      summary.Pending,
			Found:        summary.Found,
			BootOK:       bootOK,
			BootSeconds:  bootDuration.Seconds(),
			TotalSeconds: summary.totalDuration().Seconds(),
			TestSeconds:  summary.TestSecs,
			BatteryPct:   summary.BatteryPct,
		}
		id, err := m.store.Insert(run)
		if err != nil {
			log.Printf("[report] sqlite insert: %v", err)
		} else {
			if !summary.Found || summary.Failing > 0 {
				// Save logs (and screenshot) for failed or crashed runs.
				m.saveLogs(id, summary)
			}
			// Notify SSE clients about the new run.
			if m.NotifyFn != nil {
				m.NotifyFn()
			}
		}
	}
}

// saveLogs writes test and appium logs to reports/logs/<id>/ and marks the
// run in the database as having logs available.
func (m *Manager) saveLogs(runID int64, summary testRunSummary) {
	dir := fmt.Sprintf("reports/logs/%d", runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[logs] mkdir %s: %v", dir, err)
		return
	}
	wrote := false
	if len(summary.TestLog) > 0 {
		if err := os.WriteFile(dir+"/test.log", summary.TestLog, 0o644); err != nil {
			log.Printf("[logs] write test.log: %v", err)
		} else {
			wrote = true
		}
	}
	if len(summary.AppiumLog) > 0 {
		if err := os.WriteFile(dir+"/appium.log", summary.AppiumLog, 0o644); err != nil {
			log.Printf("[logs] write appium.log: %v", err)
		} else {
			wrote = true
		}
	}
	if len(summary.Screenshot) > 0 {
		if err := os.WriteFile(dir+"/screen.png", summary.Screenshot, 0o644); err != nil {
			log.Printf("[logs] write screen.png: %v", err)
		} else {
			if err := m.store.SetHasScreenshot(runID); err != nil {
				log.Printf("[logs] set has_screenshot: %v", err)
			}
		}
	}
	if wrote {
		if err := m.store.SetHasLogs(runID); err != nil {
			log.Printf("[logs] set has_logs: %v", err)
		}
		log.Printf("[logs] saved to %s/", dir)
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
