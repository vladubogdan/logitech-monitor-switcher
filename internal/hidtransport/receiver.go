// Package hidtransport finds and opens the Logitech receiver's HID++ control
// endpoint (the vendor-defined usage page 0xFF00 interface) and exposes a small
// read/write transport used by the hidpp package.
//
// It uses sstallion/go-hid, which vendors the hidapi C sources, so the receiver
// comms compile into the binary with no external library or runtime dependency.
//
// The HID++ interface exposes several top-level collections, one per report
// size: usage 0x0001 (short, report 0x10), 0x0002 (long, 0x11), 0x0004
// (very-long, 0x12). On macOS hidapi merges these into a single device path, so
// one handle sees every report. On Windows each collection is a SEPARATE device
// path, and a device answers a short request with a LONG reply that arrives on
// the 0x0002 collection — so we must open every collection and merge their
// reads, or feature/CHANGE-HOST replies are never seen.
package hidtransport

import (
	"errors"
	"fmt"
	"sync"
	"time"

	hid "github.com/sstallion/go-hid"
)

// IsTimeout reports whether err is the benign read-timeout sentinel.
func IsTimeout(err error) bool { return errors.Is(err, hid.ErrTimeout) }

const vendorLogitech = 0x046D

// knownReceiverPIDs are Logitech Unifying / Bolt / Nano receivers. We prefer
// these, but ultimately any 0x046D device exposing a 0xFF00 interface that
// answers a HID++ ping is acceptable.
var knownReceiverPIDs = map[uint16]bool{
	0xC548: true, // Bolt
	0xC52B: true, // Unifying
	0xC532: true, // Unifying (2nd gen)
	0xC534: true, // Nano receiver (2 devices)
	0xC539: true, // Lightspeed (G)
	0xC53A: true, // Powerplay
	0xC53F: true, // Lightspeed
	0xC541: true, // Lightspeed
	0xC542: true, // Nano
	0xC547: true, // Bolt variant
}

var hidInitOnce sync.Once

func ensureInit() error {
	var err error
	hidInitOnce.Do(func() { err = hid.Init() })
	return err
}

// collection is one HID++ top-level collection (a path + its usage).
type collection struct {
	path  string
	usage uint16
}

// ReceiverInfo describes a discovered receiver and all its HID++ collections.
type ReceiverInfo struct {
	VendorID  uint16
	ProductID uint16
	Product   string
	// Path is the primary (short-report) collection path, used for display.
	Path        string
	collections []collection
}

// Summary describes the receiver and its collections for logging.
func (info ReceiverInfo) Summary() string {
	s := fmt.Sprintf("PID=0x%04X collections=[", info.ProductID)
	for i, c := range info.collections {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("0x%04X", c.usage)
	}
	return s + "]"
}

// Receiver is an open HID++ transport to a receiver. It may hold several
// underlying HID handles (one per collection) whose reads are merged.
type Receiver struct {
	Info ReceiverInfo

	mu     sync.Mutex
	writeD *hid.Device   // handle used for writing short (0x10) requests
	devs   []*hid.Device // all open handles (for closing)

	reads  chan []byte
	closed chan struct{}
	wg     sync.WaitGroup
}

// Find enumerates candidate receivers (0x046D devices with a 0xFF00 interface),
// grouping each receiver's HID++ collections together.
func Find() ([]ReceiverInfo, error) {
	if err := ensureInit(); err != nil {
		return nil, err
	}
	type group struct {
		info ReceiverInfo
		seen map[string]bool
	}
	groups := map[string]*group{}
	var order []string

	err := hid.Enumerate(vendorLogitech, hid.ProductIDAny, func(di *hid.DeviceInfo) error {
		if di.UsagePage != 0xFF00 {
			return nil
		}
		// Group collections belonging to the same physical HID++ interface.
		key := fmt.Sprintf("%04x/%d", di.ProductID, di.InterfaceNbr)
		g := groups[key]
		if g == nil {
			g = &group{
				info: ReceiverInfo{
					VendorID:  di.VendorID,
					ProductID: di.ProductID,
					Product:   di.ProductStr,
				},
				seen: map[string]bool{},
			}
			groups[key] = g
			order = append(order, key)
		}
		colKey := fmt.Sprintf("%s#%04x", di.Path, di.Usage)
		if !g.seen[colKey] {
			g.seen[colKey] = true
			g.info.collections = append(g.info.collections, collection{path: di.Path, usage: di.Usage})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var known, other []ReceiverInfo
	for _, key := range order {
		info := groups[key].info
		// Primary path = the short (0x0001) collection if present, else first.
		info.Path = info.collections[0].path
		for _, c := range info.collections {
			if c.usage == 0x0001 {
				info.Path = c.path
				break
			}
		}
		if knownReceiverPIDs[info.ProductID] {
			known = append(known, info)
		} else {
			other = append(other, info)
		}
	}
	return append(known, other...), nil
}

// Open opens all of the receiver's HID++ collections and starts merged reads.
func Open(info ReceiverInfo) (*Receiver, error) {
	if err := ensureInit(); err != nil {
		return nil, err
	}
	r := &Receiver{
		Info:   info,
		reads:  make(chan []byte, 64),
		closed: make(chan struct{}),
	}

	// Open distinct paths (macOS shares one path across collections).
	openedPaths := map[string]*hid.Device{}
	for _, c := range info.collections {
		if _, ok := openedPaths[c.path]; ok {
			// Same path already open; if it is the short collection, prefer it
			// for writing.
			if c.usage == 0x0001 {
				r.writeD = openedPaths[c.path]
			}
			continue
		}
		dev, err := hid.OpenPath(c.path)
		if err != nil {
			continue // skip collections we can't open; others may still work
		}
		openedPaths[c.path] = dev
		r.devs = append(r.devs, dev)
		if r.writeD == nil || c.usage == 0x0001 {
			r.writeD = dev
		}
	}
	if len(r.devs) == 0 {
		return nil, fmt.Errorf("could not open any HID++ collection for receiver 0x%04X", info.ProductID)
	}

	// One reader goroutine per handle, merged into r.reads.
	for _, dev := range r.devs {
		d := dev
		r.wg.Add(1)
		go r.readerLoop(d)
	}
	return r, nil
}

// OpenFirst finds and opens the first candidate receiver.
func OpenFirst() (*Receiver, error) {
	list, err := Find()
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no Logitech receiver with a HID++ (0xFF00) interface found")
	}
	return Open(list[0])
}

func (r *Receiver) readerLoop(dev *hid.Device) {
	defer r.wg.Done()
	buf := make([]byte, 64)
	for {
		select {
		case <-r.closed:
			return
		default:
		}
		n, err := dev.ReadWithTimeout(buf, 300*time.Millisecond)
		if err != nil {
			if IsTimeout(err) {
				continue
			}
			return // handle closed or device gone
		}
		if n <= 0 {
			continue
		}
		rep := make([]byte, n)
		copy(rep, buf[:n])
		select {
		case r.reads <- rep:
		case <-r.closed:
			return
		}
	}
}

// Write sends a raw HID++ report on the short-report collection. The first byte
// must be the report ID (0x10 short, 0x11 long).
func (r *Receiver) Write(report []byte) error {
	r.mu.Lock()
	dev := r.writeD
	r.mu.Unlock()
	if dev == nil {
		return fmt.Errorf("receiver has no writable collection")
	}
	_, err := dev.Write(report)
	return err
}

// Read returns one HID++ report from any collection, blocking up to timeout.
// On timeout it returns hid.ErrTimeout (recognised by IsTimeout).
func (r *Receiver) Read(buf []byte, timeout time.Duration) (int, error) {
	select {
	case rep := <-r.reads:
		n := copy(buf, rep)
		return n, nil
	case <-time.After(timeout):
		return 0, hid.ErrTimeout
	case <-r.closed:
		return 0, fmt.Errorf("receiver closed")
	}
}

// Close stops the readers and closes every handle.
func (r *Receiver) Close() error {
	r.mu.Lock()
	select {
	case <-r.closed:
		r.mu.Unlock()
		return nil
	default:
		close(r.closed)
	}
	devs := r.devs
	r.mu.Unlock()

	for _, d := range devs {
		d.Close() // unblocks the reader's ReadWithTimeout
	}
	r.wg.Wait()
	return nil
}
