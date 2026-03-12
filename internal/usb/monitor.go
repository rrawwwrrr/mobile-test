package usb

import (
	"log"
	"sync"
	"time"

	"adbtest/internal/adb"
	"adbtest/internal/store"
)

// Monitor polls USB devices and records appear/disappear events to the store.
type Monitor struct {
	st   *store.Store
	prev map[string]adb.USBAndroidDevice // keyed by path
	mu   sync.Mutex
}

// NewMonitor creates a new USB Monitor.
func NewMonitor(st *store.Store) *Monitor {
	return &Monitor{
		st:   st,
		prev: make(map[string]adb.USBAndroidDevice),
	}
}

// Poll runs one diff cycle. Call periodically (e.g. on each reconcile tick).
func (m *Monitor) Poll() {
	current := adb.USBAndroidDevices()

	// Cross-reference with ADB to set in_adb flag.
	adbDevs, _ := adb.ListDevices()
	adbSerials := make(map[string]bool, len(adbDevs))
	for _, d := range adbDevs {
		adbSerials[d.Serial] = true
	}
	for i := range current {
		if current[i].Serial != "" {
			current[i].InADB = adbSerials[current[i].Serial]
		}
	}

	curMap := make(map[string]adb.USBAndroidDevice, len(current))
	for _, d := range current {
		curMap[d.Path] = d
	}

	now := time.Now().UTC()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Detect newly appeared devices.
	for path, d := range curMap {
		if _, existed := m.prev[path]; !existed {
			ev := store.USBEvent{
				TS: now, Event: "appeared",
				Path: d.Path, VID: d.VID, PID: d.PID,
				Serial: d.Serial, Product: d.Product, Vendor: d.Vendor,
				InADB: d.InADB,
			}
			if err := m.st.InsertUSBEvent(ev); err != nil {
				log.Printf("[usb] insert appeared event: %v", err)
			} else {
				log.Printf("[usb] appeared  %s  %s:%s  %s  adb=%v",
					d.Path, d.VID, d.PID, d.Product, d.InADB)
			}
		}
	}

	// Detect disappeared devices.
	for path, d := range m.prev {
		if _, exists := curMap[path]; !exists {
			ev := store.USBEvent{
				TS: now, Event: "disappeared",
				Path: d.Path, VID: d.VID, PID: d.PID,
				Serial: d.Serial, Product: d.Product, Vendor: d.Vendor,
				InADB: false,
			}
			if err := m.st.InsertUSBEvent(ev); err != nil {
				log.Printf("[usb] insert disappeared event: %v", err)
			} else {
				log.Printf("[usb] disappeared %s  %s:%s  %s",
					d.Path, d.VID, d.PID, d.Product)
			}
		}
	}

	m.prev = curMap
}
