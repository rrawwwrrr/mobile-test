package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"adbtest/internal/adb"
	"adbtest/internal/docker"
	"adbtest/internal/store"
	"adbtest/internal/web"

	dockerclient "github.com/docker/docker/client"
)

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

	var (
		watch      = flag.Bool("watch", envOrBool("ADBTEST_WATCH"), "Continuously watch for device changes [$ADBTEST_WATCH]")
		pullImage  = flag.Bool("pull", envOrBool("ADBTEST_PULL"), "Pull Appium image before starting [$ADBTEST_PULL]")
		restartADB = flag.Bool("restart-adb", envOrBool("ADBTEST_RESTART_ADB"), "Restart ADB server to listen on all interfaces [$ADBTEST_RESTART_ADB]")

		appiumImage = flag.String("image", envOr("APPIUM_IMAGE", "appium/appium:latest"), "Appium Docker image [$APPIUM_IMAGE]")
		adbHost     = flag.String("adb-host", envOr("ADB_HOST", "host.docker.internal"), "ADB server host reachable from containers [$ADB_HOST]")

		basePort = flag.Int("port", envOrInt("APPIUM_BASE_PORT", 4723), "Starting host port for Appium containers [$APPIUM_BASE_PORT]")
		adbPort  = flag.Int("adb-port", envOrInt("ADB_PORT", 5037), "ADB server port [$ADB_PORT]")

		// Test runner options.
		testImage    = flag.String("test-image", envOr("TEST_IMAGE", ""), "Docker image for test containers; empty = no tests [$TEST_IMAGE]")
		testBuildCtx = flag.String("test-build", envOr("TEST_BUILD_CONTEXT", ""), "Build test image from this directory before starting [$TEST_BUILD_CONTEXT]")

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

	cfg := docker.Config{
		AppiumImage: *appiumImage,
		TestImage:   *testImage,
		BasePort:    *basePort,
		ADBHost:     *adbHost,
		ADBPort:     *adbPort,
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

	// Optionally pull the Appium image.
	if *pullImage {
		if err := mgr.PullImage(ctx, *appiumImage); err != nil {
			log.Fatalf("Pull image: %v", err)
		}
	}

	// Start HTTP dashboard.
	webSrv := web.NewServer(st)
	mux := http.NewServeMux()
	webSrv.RegisterRoutes(mux)
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
