// Package powermon reports OS sleep/suspend and wake/resume transitions so the
// rest of the app can suspend switching across them.
//
// Why this exists: when the machine sleeps, the Logitech receiver's HID links
// to the keyboard/mouse drop, and on wake they re-establish over a second or
// two. During that churn the devices briefly look "not connected" — which is
// indistinguishable, at the HID++ level, from the user roaming to the other
// computer. Without this, waking from sleep triggers a false switch (the mouse
// gets pushed to the other host and the monitor input flips). Callers use these
// events to disarm around the transition.
package powermon

import (
	"sync"
	"time"
)

// Event is a system power transition.
type Event int

const (
	// Sleep is delivered as the system is about to suspend.
	Sleep Event = iota
	// Wake is delivered after the system resumes.
	Wake
)

func (e Event) String() string {
	if e == Wake {
		return "wake"
	}
	return "sleep"
}

var events = make(chan Event, 8)

// Events returns the channel of power transitions. It never closes.
func Events() <-chan Event { return events }

// Synchronous power state, written inside emit (i.e. directly from the OS power
// callback's thread) so any goroutine can consult it without waiting for the
// Events() channel to be drained. This is what closes the wake-race: the HID
// reconnect churn that arrives on wake is handled on a different goroutine than
// the one draining Events(), so a channel-only signal can be applied too late
// to stop a bogus switch. Reading this state directly at decide-time cannot be
// out-raced by scheduler starvation.
var (
	stateMu  sync.Mutex
	asleep   bool
	lastWake time.Time
)

// Suppressed reports whether switching should currently be held off because the
// system is asleep or woke within grace. asleep stays true from the sleep
// callback until the wake callback clears it, so there is no unsuppressed gap
// during the reconnect churn.
func Suppressed(grace time.Duration) bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	if asleep {
		return true
	}
	return !lastWake.IsZero() && time.Since(lastWake) < grace
}

// emit records the transition synchronously, then posts it without blocking
// (dropping only if the buffer is full, which means nobody is draining — moot).
func emit(e Event) {
	stateMu.Lock()
	if e == Sleep {
		asleep = true
	} else {
		asleep = false
		lastWake = time.Now()
	}
	stateMu.Unlock()

	select {
	case events <- e:
	default:
	}
}
