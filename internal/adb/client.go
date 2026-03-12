package adb

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Device represents a connected Android device.
type Device struct {
	Serial string
	State  string
	Model  string // ro.product.model, populated for ready devices
}

// IsReady returns true if the device is online and ready.
func (d Device) IsReady() bool {
	return d.State == "device"
}

// ListDevices runs `adb devices` and returns the list of detected devices.
func ListDevices() ([]Device, error) {
	out, err := exec.Command("adb", "devices").Output()
	if err != nil {
		return nil, fmt.Errorf("adb devices: %w", err)
	}

	var devices []Device
	scanner := bufio.NewScanner(strings.NewReader(string(out)))

	// First line is always "List of devices attached"
	scanner.Scan()

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		devices = append(devices, Device{
			Serial: parts[0],
			State:  parts[1],
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Populate model name for ready devices.
	for i, d := range devices {
		if !d.IsReady() {
			continue
		}
		if b, err := exec.Command("adb", "-s", d.Serial, "shell", "getprop", "ro.product.model").Output(); err == nil {
			devices[i].Model = strings.TrimSpace(string(b))
		}
	}

	return devices, nil
}

// Reboot sends `adb reboot` to the device.
func Reboot(serial string) error {
	if out, err := exec.Command("adb", "-s", serial, "reboot").CombinedOutput(); err != nil {
		return fmt.Errorf("adb reboot: %w\n%s", err, out)
	}
	return nil
}

// isOnline returns true if the device reports state "device".
func isOnline(serial string) bool {
	out, err := exec.Command("adb", "-s", serial, "get-state").Output()
	return err == nil && strings.TrimSpace(string(out)) == "device"
}

// isBootCompleted returns true when Android has finished booting
// (sys.boot_completed=1). ADB becomes reachable well before the system
// finishes starting, so checking only get-state is not enough.
func isBootCompleted(serial string) bool {
	out, err := exec.Command("adb", "-s", serial, "shell", "getprop", "sys.boot_completed").Output()
	return err == nil && strings.TrimSpace(string(out)) == "1"
}

// WaitForReady blocks until the device is in "device" state AND has fully
// booted (sys.boot_completed=1), or until timeout expires.
// It first waits (up to 30 s) for the device to go offline so we don't return
// prematurely if it hasn't actually started rebooting yet.
// Returns the total elapsed time from the moment it is called.
func WaitForReady(serial string, timeout time.Duration) (time.Duration, error) {
	start := time.Now()

	// Phase 1 – wait for the device to go offline (max 30 s).
	offlineDeadline := start.Add(30 * time.Second)
	for time.Now().Before(offlineDeadline) {
		if !isOnline(serial) {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Phase 2 – wait for the device to come back AND finish booting.
	// ADB transport becomes available long before the system is fully up;
	// sys.boot_completed=1 confirms that all system services have started.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		if isOnline(serial) && isBootCompleted(serial) {
			return time.Since(start), nil
		}
	}
	return time.Since(start), fmt.Errorf("device %s not ready after %v", serial, timeout)
}

// GrantAppiumPermissions pre-grants SYSTEM_ALERT_WINDOW and POST_NOTIFICATIONS
// to Appium helper packages so Android does not show permission dialogs during
// test execution. Errors are silently ignored — the packages may not be
// installed on the very first run (Appium installs them during its first
// session); from the second run onwards the permission will already be set.
func GrantAppiumPermissions(serial string) {
	pkgs := []string{
		"io.appium.settings",
		"io.appium.uiautomator2.server",
		"io.appium.uiautomator2.server.test",
		"io.appium.android.apis",
	}
	granted := 0
	for _, pkg := range pkgs {
		if err := exec.Command("adb", "-s", serial, "shell",
			"appops", "set", pkg, "SYSTEM_ALERT_WINDOW", "allow").Run(); err == nil {
			granted++
		}
	}
	// POST_NOTIFICATIONS (Android 13+): only for packages that declare the
	// permission in their manifest. Appium internal packages do not declare it,
	// so pm grant would throw SecurityException — skip them.
	for _, pkg := range []string{"io.appium.android.apis"} {
		_ = exec.Command("adb", "-s", serial, "shell",
			"pm", "grant", pkg, "android.permission.POST_NOTIFICATIONS").Run()
	}
	if granted > 0 {
		log.Printf("[appium] granted SYSTEM_ALERT_WINDOW to %d package(s) on %s", granted, serial)
	}
	// Set default USB function to MTP so Android doesn't show the
	// "USB-подключение" mode-selection dialog after reboot.
	_ = exec.Command("adb", "-s", serial, "shell", "svc", "usb", "setFunctions", "mtp").Run()
}

// androidVendors maps USB vendor IDs (lowercase hex) to OEM names.
var androidVendors = map[string]string{
	"0489": "Foxconn",
	"04c5": "Fujitsu",
	"04dd": "Sharp",
	"04e8": "Samsung",
	"0502": "Acer",
	"05c6": "Qualcomm",
	"0b05": "Asus",
	"0bb4": "HTC",
	"0e8d": "Tecno/Infinix",
	"0fce": "Sony Ericsson",
	"1004": "LG",
	"12d1": "Huawei",
	"17ef": "Lenovo",
	"18d1": "Google",
	"19d2": "ZTE",
	"1782": "Unisoc",
	"1bbb": "Alcatel",
	"1d4d": "Pegatron",
	"1ebf": "Huawei",
	"1f3a": "Xiaomi",
	"22b8": "Motorola",
	"22d9": "Realme",
	"2717": "Xiaomi/Poco/Redmi",
	"2a45": "Meizu",
	"2a70": "OPPO/Realme/OnePlus",
	"2d95": "vivo",
	"2e04": "Nokia",
	"2ee5": "Fairphone",
	"339b": "Honor",
}

// USBAndroidDevice describes an Android device detected via USB sysfs,
// regardless of whether ADB can see it.
type USBAndroidDevice struct {
	Path    string `json:"path"`
	VID     string `json:"vid"`
	PID     string `json:"pid"`
	Serial  string `json:"serial"`   // USB serial descriptor (may be empty)
	Product string `json:"product"`  // USB product string
	Vendor  string `json:"vendor"`   // human-readable OEM name
	InADB   bool   `json:"in_adb"`  // true if visible in `adb devices`
}

// USBAndroidDevices enumerates all USB devices with known Android vendor IDs
// by reading /sys/bus/usb/devices. Only device nodes (no interface suffixes)
// are returned.
func USBAndroidDevices() []USBAndroidDevice {
	entries, err := os.ReadDir("/sys/bus/usb/devices")
	if err != nil {
		return nil
	}
	var out []USBAndroidDevice
	for _, entry := range entries {
		name := entry.Name()
		// Skip interface nodes (e.g. "2-1.3:1.0") — device nodes have no colon.
		if strings.ContainsRune(name, ':') {
			continue
		}
		dir := "/sys/bus/usb/devices/" + name
		vidB, err := os.ReadFile(dir + "/idVendor")
		if err != nil {
			continue
		}
		vid := strings.TrimSpace(string(vidB))
		oem, ok := androidVendors[vid]
		if !ok {
			continue
		}
		dev := USBAndroidDevice{Path: name, VID: vid, Vendor: oem}
		if b, err := os.ReadFile(dir + "/idProduct"); err == nil {
			dev.PID = strings.TrimSpace(string(b))
		}
		if b, err := os.ReadFile(dir + "/serial"); err == nil {
			dev.Serial = strings.TrimSpace(string(b))
		}
		if b, err := os.ReadFile(dir + "/product"); err == nil {
			dev.Product = strings.TrimSpace(string(b))
		}
		out = append(out, dev)
	}
	return out
}

// USBInfo returns the sysfs USB path, vendor ID and product ID for the device
// with the given ADB serial by scanning /sys/bus/usb/devices/. Returns empty
// strings if the device is not found (e.g. already disconnected).
func USBInfo(serial string) (path, vid, pid string) {
	entries, err := os.ReadDir("/sys/bus/usb/devices")
	if err != nil {
		return
	}
	for _, entry := range entries {
		dir := "/sys/bus/usb/devices/" + entry.Name()
		b, err := os.ReadFile(dir + "/serial")
		if err != nil || strings.TrimSpace(string(b)) != serial {
			continue
		}
		path = entry.Name()
		if b, err := os.ReadFile(dir + "/idVendor"); err == nil {
			vid = strings.TrimSpace(string(b))
		}
		if b, err := os.ReadFile(dir + "/idProduct"); err == nil {
			pid = strings.TrimSpace(string(b))
		}
		return
	}
	return
}

// BatteryLevel returns the current battery charge level (0–100) for the device.
// Returns -1 and a non-nil error if the level cannot be determined.
func BatteryLevel(serial string) (int, error) {
	out, err := exec.Command("adb", "-s", serial, "shell", "dumpsys", "battery").Output()
	if err != nil {
		return -1, fmt.Errorf("adb dumpsys battery: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "level:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "level:"))
			n, err := strconv.Atoi(val)
			if err != nil {
				return -1, fmt.Errorf("parse battery level %q: %w", val, err)
			}
			return n, nil
		}
	}
	return -1, fmt.Errorf("battery level not found in dumpsys output")
}

// EnsureServerListensOnAllInterfaces restarts the ADB server with the -a flag
// so containers can connect to it over the Docker bridge network.
func EnsureServerListensOnAllInterfaces() error {
	log.Println("Restarting ADB server to listen on all interfaces (-a)...")

	// Kill existing server
	if out, err := exec.Command("adb", "kill-server").CombinedOutput(); err != nil {
		return fmt.Errorf("adb kill-server: %w\n%s", err, out)
	}

	// Start new server listening on all interfaces
	cmd := exec.Command("adb", "-a", "-P", "5037", "nodaemon", "server", "start")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("adb start server: %w", err)
	}

	log.Printf("ADB server started in background (pid %d)", cmd.Process.Pid)
	return nil
}
