//go:build darwin

package powermon

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <IOKit/IOMessage.h>
#include <CoreFoundation/CoreFoundation.h>

extern void goPowerEvent(int kind);

static io_connect_t gRootPort;

// powerCallback receives IOKit power-management messages on the run loop this
// registered with.
static void powerCallback(void *refcon, io_service_t service,
                          natural_t messageType, void *messageArgument) {
    switch (messageType) {
    case kIOMessageCanSystemSleep:
        // A request we could veto; allow it so we never block sleep.
        IOAllowPowerChange(gRootPort, (long)messageArgument);
        break;
    case kIOMessageSystemWillSleep:
        goPowerEvent(0); // sleep
        // Must acknowledge or the system waits ~30s for us before sleeping.
        IOAllowPowerChange(gRootPort, (long)messageArgument);
        break;
    case kIOMessageSystemHasPoweredOn:
        goPowerEvent(1); // wake
        break;
    default:
        break;
    }
}

// runPowerLoop registers for system power notifications and runs the current
// thread's CFRunLoop forever to service them. Meant to run on its own thread.
static void runPowerLoop(void) {
    IONotificationPortRef port = NULL;
    io_object_t notifier = 0;
    gRootPort = IORegisterForSystemPower(NULL, &port, powerCallback, &notifier);
    if (gRootPort == MACH_PORT_NULL) {
        return;
    }
    CFRunLoopAddSource(CFRunLoopGetCurrent(),
                       IONotificationPortGetRunLoopSource(port),
                       kCFRunLoopCommonModes);
    CFRunLoopRun();
}
*/
import "C"

import (
	"runtime"
	"sync"
)

//export goPowerEvent
func goPowerEvent(kind C.int) {
	if kind == 1 {
		emit(Wake)
	} else {
		emit(Sleep)
	}
}

var startOnce sync.Once

// Start begins watching for power events on a dedicated OS thread running its
// own CFRunLoop. Idempotent.
func Start() {
	startOnce.Do(func() {
		go func() {
			// The CFRunLoop is thread-affine, so pin this goroutine.
			runtime.LockOSThread()
			C.runPowerLoop()
		}()
	})
}
