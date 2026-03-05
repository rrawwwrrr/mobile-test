package adb

import (
	"bufio"
	"fmt"
	"log"
	"os/exec"
	"strings"
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
