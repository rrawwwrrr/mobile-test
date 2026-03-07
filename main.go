package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"adbtest/internal/adb"
	"adbtest/internal/docker"
	"adbtest/internal/store"
	"adbtest/internal/web"

	dockerclient "github.com/docker/docker/client"
)

// version is injected at build time via -ldflags "-X main.version=vX.Y.Z".
// Falls back to "dev" for local builds.
var version = "dev"

// envOr returns the value of the environment variable if set, otherwise the fallback.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envOrInt returns the integer value of the environment variable if set and valid,
// otherwise the fallback.
func envOrInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("Warning: invalid integer value for %s=%q, using default %d", key, v, fallback)
	}
	return fallback
}

// envOrBool returns true if the environment variable is set to "1", "true", or "yes".
func envOrBool(key string) bool {
	switch os.Getenv(key) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// ensureAPK downloads the APK from url to path if the file doesn't already exist.
func ensureAPK(path, url string) error {
	if _, err := os.Stat(path); err == nil {
		log.Printf("[apk] using cached %s", path)
		return nil
	}
	if url == "" {
		return fmt.Errorf("APK not found at %s and no --apk-url provided", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	log.Printf("[apk] downloading %s → %s", url, path)
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write: %w", err)
	}
	f.Close()
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	log.Printf("[apk] saved to %s", path)
	return nil
}

// defaultTestImage returns the versioned test image for release builds,
// or empty string for dev builds (tests disabled unless --test-image is passed).
func defaultTestImage() string {
	if version == "dev" {
		return ""
	}
	return "rrawwwrrr/adbtest-tests:" + strings.TrimPrefix(version, "v")
}

func main() {
	// Environment variables serve as defaults; CLI flags override them.
	//
	// Supported env vars:
	//   APPIUM_IMAGE        – Docker image for Appium        (default: appium/appium:latest)
	//   APPIUM_BASE_PORT    – Starting host port             (default: 4723)
	//   ADB_HOST            – ADB server host for containers (default: host.docker.internal)
	//   ADB_PORT            – ADB server port                (default: 5037)
	//   TEST_IMAGE          – Docker image for test runner   (default: "", disabled)
	//   TEST_BUILD_CONTEXT  – Path to Dockerfile dir to build test image from
	//   ADBTEST_WATCH       – Enable watch mode              (1/true/yes)
	//   ADBTEST_INTERVAL    – Poll interval, e.g. "10s"      (default: 5s)
	//   ADBTEST_PULL        – Pull Appium image before start (1/true/yes)
	//   ADBTEST_RESTART_ADB – Restart ADB on all interfaces  (1/true/yes)
	//   ADBTEST_HTTP_ADDR   – HTTP dashboard listen address  (default: :8080)
	//   ADBTEST_DB          – SQLite database path           (default: reports/adbtest.db)
	//   ADBTEST_APK         – Local APK file path            (default: apk/ApiDemos-debug.apk)
	//   ADBTEST_APK_URL     – URL to download APK if missing (default: github release)

	const defaultAPKURL = "https://github.com/appium/android-apidemos/releases/download/v6.0.6/ApiDemos-debug.apk"

	var (
		watch      = flag.Bool("watch", envOrBool("ADBTEST_WATCH"), "Continuously watch for device changes [$ADBTEST_WATCH]")
		pullImage  = flag.Bool("pull", envOrBool("ADBTEST_PULL"), "Pull Appium image before starting [$ADBTEST_PULL]")
		restartADB = flag.Bool("restart-adb", envOrBool("ADBTEST_RESTART_ADB"), "Restart ADB server to listen on all interfaces [$ADBTEST_RESTART_ADB]")

		appiumImage = flag.String("image", envOr("APPIUM_IMAGE", "appium/appium:latest"), "Appium Docker image [$APPIUM_IMAGE]")
		adbHost     = flag.String("adb-host", envOr("ADB_HOST", "host.docker.internal"), "ADB server host reachable from containers [$ADB_HOST]")

		basePort = flag.Int("port", envOrInt("APPIUM_BASE_PORT", 4723), "Starting host port for Appium containers [$APPIUM_BASE_PORT]")
		adbPort  = flag.Int("adb-port", envOrInt("ADB_PORT", 5037), "ADB server port [$ADB_PORT]")

		// Test runner options.
		// Default test image uses the release version tag so the binary and
		// the test image are always in sync. Falls back to empty on dev builds.
		testImage    = flag.String("test-image", envOr("TEST_IMAGE", defaultTestImage()), "Docker image for test containers; empty = no tests [$TEST_IMAGE]")
		testBuildCtx = flag.String("test-build", envOr("TEST_BUILD_CONTEXT", ""), "Build test image from this directory before starting [$TEST_BUILD_CONTEXT]")

		// APK options.
		apkPath = flag.String("apk", envOr("ADBTEST_APK", "apk/ApiDemos-debug.apk"), "Local APK file path; downloaded from --apk-url if missing [$ADBTEST_APK]")
		apkURL  = flag.String("apk-url", envOr("ADBTEST_APK_URL", defaultAPKURL), "URL to download APK when --apk file is missing [$ADBTEST_APK_URL]")

		// Dashboard options.
		httpAddr = flag.String("http-addr", envOr("ADBTEST_HTTP_ADDR", ":8080"), "HTTP dashboard listen address [$ADBTEST_HTTP_ADDR]")
		dbPath   = flag.String("db", envOr("ADBTEST_DB", "reports/adbtest.db"), "SQLite database path [$ADBTEST_DB]")
	)

	// interval needs special handling because it is a duration.
	intervalDefault := 5 * time.Second
	if v := os.Getenv("ADBTEST_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			intervalDefault = d
		} else {
			log.Printf("Warning: invalid duration for ADBTEST_INTERVAL=%q, using default %s", v, intervalDefault)
		}
	}
	interval := flag.Duration("interval", intervalDefault, "Poll interval in watch mode [$ADBTEST_INTERVAL]")

	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("adbtest ")

	if *restartADB {
		if err := adb.EnsureServerListensOnAllInterfaces(); err != nil {
			log.Fatalf("Failed to restart ADB server: %v", err)
		}
		time.Sleep(time.Second)
	}

	// Ensure APK is available locally (download if needed).
	absAPK, err := filepath.Abs(*apkPath)
	if err != nil {
		log.Fatalf("APK path: %v", err)
	}
	if err := ensureAPK(absAPK, *apkURL); err != nil {
		log.Fatalf("APK: %v", err)
	}

	// Open SQLite store.
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("Store: %v", err)
	}
	defer st.Close()

	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		log.Fatalf("Docker client: %v", err)
	}
	defer cli.Close()

	// Build the URL where Appium can fetch the APK.
	// Appium containers use --network=host, so "localhost" resolves to the host.
	apkServeURL := fmt.Sprintf("http://localhost%s/apk/%s", *httpAddr, filepath.Base(absAPK))

	cfg := docker.Config{
		AppiumImage: *appiumImage,
		TestImage:   *testImage,
		BasePort:    *basePort,
		ADBHost:     *adbHost,
		ADBPort:     *adbPort,
		APKServeURL: apkServeURL,
	}
	mgr := docker.NewManager(cli, cfg, st)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Optionally build test image from a local Dockerfile directory.
	if *testBuildCtx != "" {
		tag := *testImage
		if tag == "" {
			tag = "adbtest-tests:latest"
			cfg.TestImage = tag
		}
		if err := mgr.BuildTestImage(ctx, *testBuildCtx, tag); err != nil {
			log.Fatalf("Build test image: %v", err)
		}
	}

	// Optionally pull Docker images before starting.
	if *pullImage {
		if err := mgr.PullImage(ctx, *appiumImage); err != nil {
			log.Fatalf("Pull appium image: %v", err)
		}
		if cfg.TestImage != "" {
			if err := mgr.PullImage(ctx, cfg.TestImage); err != nil {
				log.Fatalf("Pull test image: %v", err)
			}
		}
	}

	// Start HTTP dashboard + APK file server.
	hub := web.NewHub()
	webSrv := web.NewServer(st, hub)
	mgr.NotifyFn = hub.Notify
	mux := http.NewServeMux()
	webSrv.RegisterRoutes(mux)
	webSrv.ServeAPKDir(mux, filepath.Dir(absAPK))
	webSrv.ServeLogsDir(mux, "reports/logs")
	httpServer := &http.Server{Addr: *httpAddr, Handler: mux}
	go func() {
		log.Printf("Dashboard listening on http://%s", *httpAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server: %v", err)
		}
	}()
	defer httpServer.Shutdown(context.Background())

	if *watch {
		log.Printf("Watch mode (interval=%s, appium=%s, tests=%s, base-port=%d)",
			*interval, *appiumImage, cfg.TestImage, *basePort)
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()

		reconcile(ctx, mgr)
		for {
			select {
			case <-ctx.Done():
				log.Println("Shutting down.")
				return
			case <-ticker.C:
				reconcile(ctx, mgr)
			}
		}
	} else {
		reconcile(ctx, mgr)
	}
}

func reconcile(ctx context.Context, mgr *docker.Manager) {
	devices, err := adb.ListDevices()
	if err != nil {
		log.Printf("adb list devices: %v", err)
		return
	}

	ready := 0
	for _, d := range devices {
		if d.IsReady() {
			ready++
		}
	}
	log.Printf("Detected %d device(s) total, %d ready.", len(devices), ready)

	if err := mgr.Reconcile(ctx, devices); err != nil {
		log.Printf("Reconcile error: %v", err)
	}
}
