// Package hidpp speaks the Logitech HID++ protocol to a receiver: it enables
// and dispatches device-connection notifications (used to detect a keyboard or
// mouse roaming to another host) and issues the HID++ 2.0 CHANGE HOST command
// (used to push the mouse to follow the keyboard).
//
// A single background goroutine reads every report from the receiver and routes
// it either to a pending request or to the notification channel. Requests are
// serialized, so at most one is outstanding at a time.
package hidpp

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"logimonitorswitch/internal/hidtransport"
)

// SoftwareID tags our requests so their replies can be told apart from
// unsolicited notifications.
const SoftwareID = 0x0A

// Report IDs.
const (
	reportShort = 0x10 // 7 bytes
	reportLong  = 0x11 // 20 bytes
)

// Receiver "device index" for register access is 0xFF.
const receiverIndex = 0xFF

// HID++ 2.0 feature IDs we use.
const (
	featureRoot       = 0x0000
	featureDeviceType = 0x0005
	featureChangeHost = 0x1814
)

// Device types reported by feature 0x0005 function 2.
const (
	DeviceKeyboard = 0
	DeviceMouse    = 3
)

// ErrClosed is returned once the connection has been closed.
var ErrClosed = errors.New("hidpp: connection closed")

// ErrTimeout is returned when a request gets no matching reply in time.
var ErrTimeout = errors.New("hidpp: request timed out")

// HIDPPError is a protocol-level error reply (HID++ 1.0 0x8F or 2.0 0xFF).
type HIDPPError struct {
	Code byte
}

func (e HIDPPError) Error() string { return fmt.Sprintf("hidpp: device error 0x%02X", e.Code) }

// Notification is an unsolicited report from the receiver/device.
type Notification struct {
	Device byte   // device index (1..6), 0xFF = receiver
	SubID  byte   // e.g. 0x41 device-connection
	Params []byte // bytes after the sub-id
	Raw    []byte
}

// Conn is an open HID++ connection over a receiver.
type Conn struct {
	rcv *hidtransport.Receiver

	notifyCh chan Notification
	respCh   chan []byte

	reqMu sync.Mutex // serializes requests

	matchMu  sync.Mutex
	curMatch func([]byte) bool

	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup
}

// Open wraps a receiver and starts the dispatcher.
func Open(rcv *hidtransport.Receiver) *Conn {
	c := &Conn{
		rcv:      rcv,
		notifyCh: make(chan Notification, 32),
		respCh:   make(chan []byte, 4),
		closed:   make(chan struct{}),
	}
	c.wg.Add(1)
	go c.readLoop()
	return c
}

// Notifications returns the channel of unsolicited reports.
func (c *Conn) Notifications() <-chan Notification { return c.notifyCh }

// Close stops the dispatcher and closes the receiver.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	err := c.rcv.Close() // unblocks the pending Read
	c.wg.Wait()
	return err
}

func (c *Conn) setMatch(m func([]byte) bool) {
	c.matchMu.Lock()
	c.curMatch = m
	c.matchMu.Unlock()
}

func (c *Conn) readLoop() {
	defer c.wg.Done()
	defer close(c.notifyCh)
	buf := make([]byte, 64)
	for {
		select {
		case <-c.closed:
			return
		default:
		}
		n, err := c.rcv.Read(buf, 300*time.Millisecond)
		if err != nil {
			if hidtransport.IsTimeout(err) {
				continue
			}
			// Device gone / unplugged: stop.
			select {
			case <-c.closed:
			default:
			}
			return
		}
		if n == 0 {
			continue
		}
		rep := make([]byte, n)
		copy(rep, buf[:n])

		c.matchMu.Lock()
		m := c.curMatch
		c.matchMu.Unlock()
		if m != nil && m(rep) {
			select {
			case c.respCh <- rep:
			default:
			}
			continue
		}
		// Unsolicited notification.
		note := Notification{Raw: rep}
		if len(rep) >= 3 {
			note.Device = rep[1]
			note.SubID = rep[2]
			note.Params = rep[3:]
		}
		select {
		case c.notifyCh <- note:
		default: // drop if nobody is listening fast enough
		}
	}
}

// isErrorReply reports whether rep is a HID++ error frame and its code.
func isErrorReply(rep []byte) (bool, byte) {
	if len(rep) >= 6 && rep[0] == reportShort {
		if rep[2] == 0x8F { // HID++ 1.0 error
			return true, rep[5]
		}
		if rep[2] == 0xFF { // HID++ 2.0 error
			return true, rep[5]
		}
	}
	return false, 0
}

// matcherFor builds a predicate that recognises the reply to req.
func matcherFor(req []byte) func([]byte) bool {
	dev, b2, b3 := req[1], req[2], req[3]
	return func(rep []byte) bool {
		if len(rep) < 4 || rep[1] != dev {
			return false
		}
		// Normal echo (works for both 1.0 register and 2.0 feature calls).
		if rep[2] == b2 && rep[3] == b3 {
			return true
		}
		// Error frames echo the original b2/b3 shifted by one.
		if (rep[2] == 0x8F || rep[2] == 0xFF) && len(rep) >= 5 && rep[3] == b2 && rep[4] == b3 {
			return true
		}
		return false
	}
}

// request sends req and waits for its reply.
func (c *Conn) request(req []byte, timeout time.Duration) ([]byte, error) {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()

	select {
	case <-c.closed:
		return nil, ErrClosed
	default:
	}

	c.setMatch(matcherFor(req))
	defer c.setMatch(nil)

	// Drain any stale response.
	select {
	case <-c.respCh:
	default:
	}

	if err := c.rcv.Write(req); err != nil {
		return nil, err
	}
	select {
	case rep := <-c.respCh:
		if isErr, code := isErrorReply(rep); isErr {
			return rep, HIDPPError{Code: code}
		}
		return rep, nil
	case <-time.After(timeout):
		return nil, ErrTimeout
	case <-c.closed:
		return nil, ErrClosed
	}
}

func funcSw(fn byte) byte { return (fn << 4) | SoftwareID }

// transient reports whether err is worth retrying. Since we open the receiver
// non-exclusively (so Logi Options+ / Flow keeps working alongside us), another
// process can be mid-transaction on the same device when we ask, making a
// request momentarily time out or come back BUSY/UNSUPPORTED.
func transient(err error) bool {
	if errors.Is(err, ErrTimeout) {
		return true
	}
	var he HIDPPError
	if errors.As(err, &he) {
		return he.Code == 0x08 || he.Code == 0x09 // BUSY / (transient) UNSUPPORTED
	}
	return false
}

// withRetry runs op up to n times, backing off briefly between attempts as long
// as the failure looks like receiver contention. Non-transient errors (e.g. a
// device that genuinely lacks a feature) return immediately.
func (c *Conn) withRetry(n int, op func() error) error {
	var err error
	for i := 0; i < n; i++ {
		if err = op(); err == nil || !transient(err) {
			return err
		}
		time.Sleep(120 * time.Millisecond)
	}
	return err
}

// --- High-level operations ------------------------------------------------

// EnableNotifications turns on wireless device-connection notifications on the
// receiver (register 0x00, wireless bit).
func (c *Conn) EnableNotifications() error {
	_, err := c.request([]byte{reportShort, receiverIndex, 0x80, 0x00, 0x00, 0x01, 0x00}, time.Second)
	return err
}

// Enumerate asks the receiver to re-emit a connection notification for every
// paired device (register 0x02 = 0x02). The notifications arrive on
// Notifications().
func (c *Conn) Enumerate() error {
	_, err := c.request([]byte{reportShort, receiverIndex, 0x80, 0x02, 0x02, 0x00, 0x00}, time.Second)
	return err
}

// Ping reports whether device dev is currently connected, and its HID++
// protocol version (major.minor) when it is.
func (c *Conn) Ping(dev byte) (connected bool, major, minor byte) {
	return c.pingTimeout(dev, 500*time.Millisecond)
}

// PingWithTimeout reports whether dev is connected, waiting at most timeout for
// the reply. A present device answers within a few milliseconds, so a short
// timeout makes "is it gone?" checks fast: an absent device costs only timeout
// instead of the default half second.
func (c *Conn) PingWithTimeout(dev byte, timeout time.Duration) bool {
	ok, _, _ := c.pingTimeout(dev, timeout)
	return ok
}

func (c *Conn) pingTimeout(dev byte, timeout time.Duration) (connected bool, major, minor byte) {
	rep, err := c.request([]byte{reportShort, dev, featureRoot, funcSw(1), 0x00, 0x00, 0xAA}, timeout)
	if err != nil {
		return false, 0, 0
	}
	if len(rep) >= 6 {
		return true, rep[4], rep[5]
	}
	return true, 0, 0
}

// FeatureIndex returns the feature index for featureID on dev, or 0 if the
// device does not expose it (index 0 is always the root feature).
func (c *Conn) FeatureIndex(dev byte, featureID uint16) (byte, error) {
	req := []byte{reportShort, dev, featureRoot, funcSw(0), byte(featureID >> 8), byte(featureID), 0x00}
	rep, err := c.request(req, 500*time.Millisecond)
	if err != nil {
		return 0, err
	}
	if len(rep) < 5 {
		return 0, fmt.Errorf("hidpp: short getFeature reply")
	}
	return rep[4], nil
}

// DeviceType returns the feature-0x0005 device type (DeviceKeyboard, DeviceMouse, …).
func (c *Conn) DeviceType(dev byte) (byte, error) {
	idx, err := c.FeatureIndex(dev, featureDeviceType)
	if err != nil {
		return 0, err
	}
	if idx == 0 {
		return 0, fmt.Errorf("hidpp: device %d lacks feature 0x0005", dev)
	}
	rep, err := c.request([]byte{reportShort, dev, idx, funcSw(2), 0x00, 0x00, 0x00}, 500*time.Millisecond)
	if err != nil {
		return 0, err
	}
	if len(rep) < 5 {
		return 0, fmt.Errorf("hidpp: short deviceType reply")
	}
	return rep[4], nil
}

// HostInfo reports the number of host slots and the current one (0-based) for a
// device that supports feature 0x1814 (CHANGE HOST).
func (c *Conn) HostInfo(dev byte) (nbHosts, currentHost byte, err error) {
	err = c.withRetry(4, func() error {
		idx, e := c.FeatureIndex(dev, featureChangeHost)
		if e != nil {
			return e
		}
		if idx == 0 {
			return fmt.Errorf("hidpp: device %d lacks feature 0x1814 (CHANGE HOST)", dev)
		}
		rep, e := c.request([]byte{reportShort, dev, idx, funcSw(0), 0x00, 0x00, 0x00}, 500*time.Millisecond)
		if e != nil {
			return e
		}
		if len(rep) < 6 {
			return fmt.Errorf("hidpp: short hostInfo reply")
		}
		nbHosts, currentHost = rep[4], rep[5]
		return nil
	})
	return
}

// SetHost commands device dev to switch to host slot hostIndex (0-based). The
// device usually drops off this receiver immediately, so a timeout on the reply
// is treated as success.
func (c *Conn) SetHost(dev, hostIndex byte) error {
	return c.withRetry(4, func() error {
		idx, err := c.FeatureIndex(dev, featureChangeHost)
		if err != nil {
			return err
		}
		if idx == 0 {
			return fmt.Errorf("hidpp: device %d lacks feature 0x1814 (CHANGE HOST)", dev)
		}
		// The write itself triggers the host change; the reply is only an ack the
		// departing device usually never sends. So we wait only briefly for it and
		// treat a timeout as success — no need to hold up the switch for 600ms.
		_, err = c.request([]byte{reportShort, dev, idx, funcSw(1), hostIndex, 0x00, 0x00}, 300*time.Millisecond)
		if err != nil && errors.Is(err, ErrTimeout) {
			return nil // expected: the device left before replying
		}
		return err
	})
}
