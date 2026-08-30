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

// emit posts an event without blocking (drops if the buffer is full, which
// only happens if nobody is draining — in which case the event is moot).
func emit(e Event) {
	select {
	case events <- e:
	default:
	}
}
