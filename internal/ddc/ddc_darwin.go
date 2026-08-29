//go:build darwin

package ddc

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

// --- Private IOAVService API (same one m1ddc uses) ------------------------
// These live in IOKit but are not in the public headers, so declare them.
typedef CFTypeRef IOAVServiceRef;
extern IOAVServiceRef IOAVServiceCreate(CFAllocatorRef allocator);
extern IOAVServiceRef IOAVServiceCreateWithService(CFAllocatorRef allocator, io_service_t service);
extern IOReturn IOAVServiceWriteI2C(IOAVServiceRef service, uint32_t chipAddress, uint32_t offset, void *inputBuffer, uint32_t inputBufferSize);
extern IOReturn IOAVServiceReadI2C(IOAVServiceRef service, uint32_t chipAddress, uint32_t offset, void *outputBuffer, uint32_t outputBufferSize);

// Set VCP feature `vcp` to 16-bit `value` on an external display.
//
// The command is sent twice: on Apple Silicon the FIRST "set" issued through a
// freshly obtained AV service in a process is routinely dropped (the write path
// is "cold" until a set warms it), which otherwise makes the first monitor
// switch after launch silently no-op. Re-sending is idempotent for input source.
static IOReturn ddc_set(IOAVServiceRef svc, uint8_t vcp, uint16_t value) {
    if (svc == NULL) return kIOReturnBadArgument;
    char data[6];
    data[0] = 0x84;                       // length: 0x80 | (4 data bytes)
    data[1] = 0x03;                       // "set VCP feature" opcode
    data[2] = vcp;                        // feature code (0x60 = input source)
    data[3] = (value >> 8) & 0xFF;        // value high byte
    data[4] = value & 0xFF;               // value low byte
    data[5] = 0x6E ^ 0x51 ^ data[0] ^ data[1] ^ data[2] ^ data[3] ^ data[4];
    IOReturn r = IOAVServiceWriteI2C(svc, 0x37, 0x51, data, 6);
    usleep(50000);
    r = IOAVServiceWriteI2C(svc, 0x37, 0x51, data, 6); // second one actually lands
    usleep(20000);
    return r;
}

// Debug: fill up to 12 raw reply bytes for a get-VCP request.
static IOReturn ddc_get_raw(IOAVServiceRef svc, uint8_t vcp, unsigned char *out) {
    if (svc == NULL) return kIOReturnBadArgument;
    char req[4];
    req[0] = 0x82; req[1] = 0x01; req[2] = vcp;
    req[3] = 0x6E ^ 0x51 ^ req[0] ^ req[1] ^ req[2];
    IOReturn r = IOAVServiceWriteI2C(svc, 0x37, 0x51, req, 4);
    if (r != kIOReturnSuccess) return r;
    usleep(40000);
    return IOAVServiceReadI2C(svc, 0x37, 0x51, out, 12);
}

// Debug: write a get-VCP(0x60) request then read `n` bytes using read offset
// `roffset`. Returns the IOReturn; fills out with raw bytes.
static IOReturn ddc_read_dbg(IOAVServiceRef svc, uint32_t roffset, unsigned char *out, int n) {
    if (svc == NULL) return kIOReturnBadArgument;
    char req[4];
    req[0] = 0x82; req[1] = 0x01; req[2] = 0x60;
    req[3] = 0x6E ^ 0x51 ^ req[0] ^ req[1] ^ req[2];
    IOReturn w = IOAVServiceWriteI2C(svc, 0x37, 0x51, req, 4);
    if (w != kIOReturnSuccess) return w;
    usleep(50000);
    return IOAVServiceReadI2C(svc, 0x37, roffset, out, n);
}

// Locate the DDC reply tag `op` in a buffer that starts at (or near) the 0x6E
// source address. Returns the index of the tag, or -1. Handles panels that
// prefix a null/padding byte differently.
static int ddc_find_tag(unsigned char *b, int n, unsigned char op) {
    for (int i = 0; i + 1 < n; i++) {
        // A real reply frame looks like: [6E][len(0x80|k)][op]...
        if (b[i] == 0x6E && (b[i+1] & 0x80) && b[i+2] == op) return i + 2;
    }
    // Fallback: bare op tag scan.
    for (int i = 0; i < n; i++) if (b[i] == op) return i;
    return -1;
}

// Read the DDC capabilities string into out (multi-fragment, with retries for
// the "null message" the panel returns while it prepares each fragment).
// Returns the number of bytes read, or -1 on failure.
static int ddc_caps(IOAVServiceRef svc, char *out, int outCap) {
    if (svc == NULL) return -1;
    int total = 0;
    for (int offset = 0; offset < 4096; ) {
        int got = 0;
        for (int attempt = 0; attempt < 6 && !got; attempt++) {
            char req[5];
            req[0] = 0x83;                 // length: 0x80 | 3 data bytes
            req[1] = 0xF3;                 // capabilities request opcode
            req[2] = (offset >> 8) & 0xFF;
            req[3] = offset & 0xFF;
            req[4] = 0x6E ^ 0x51 ^ req[0] ^ req[1] ^ req[2] ^ req[3];
            if (IOAVServiceWriteI2C(svc, 0x37, 0x51, req, 5) != kIOReturnSuccess) { usleep(40000); continue; }
            usleep(50000);
            unsigned char reply[64];
            memset(reply, 0, sizeof(reply));
            if (IOAVServiceReadI2C(svc, 0x37, 0x51, reply, sizeof(reply)) != kIOReturnSuccess) { usleep(40000); continue; }
            int tag = ddc_find_tag(reply, sizeof(reply), 0xE3);
            if (tag < 2) { usleep(40000); continue; }    // null message; retry
            int len = reply[tag - 1] & 0x7F;             // total payload length
            int count = len - 3;                         // minus opcode + 2 offset
            unsigned char oh = reply[tag + 1], ol = reply[tag + 2];
            if (oh != ((offset >> 8) & 0xFF) || ol != (offset & 0xFF)) { usleep(40000); continue; }
            if (count <= 0) { got = 1; offset = 4096; break; } // done, no more data
            for (int i = 0; i < count && total < outCap - 1; i++) out[total++] = reply[tag + 3 + i];
            offset += count;
            got = 1;
        }
        if (!got) return total > 0 ? total : -1; // give up; return what we have
    }
    out[total] = 0;
    return total;
}

// Get VCP feature `vcp`; returns 0 on success and fills *cur/*max. Retries to
// ride out the null messages the panel emits while preparing the reply.
static IOReturn ddc_get(IOAVServiceRef svc, uint8_t vcp, uint16_t *cur, uint16_t *max) {
    if (svc == NULL) return kIOReturnBadArgument;
    for (int attempt = 0; attempt < 6; attempt++) {
        char req[4];
        req[0] = 0x82;                       // length: 0x80 | 2 data bytes
        req[1] = 0x01;                        // get VCP feature
        req[2] = vcp;
        req[3] = 0x6E ^ 0x51 ^ req[0] ^ req[1] ^ req[2];
        if (IOAVServiceWriteI2C(svc, 0x37, 0x51, req, 4) != kIOReturnSuccess) { usleep(40000); continue; }
        usleep(50000);
        unsigned char reply[16];
        memset(reply, 0, sizeof(reply));
        if (IOAVServiceReadI2C(svc, 0x37, 0x51, reply, sizeof(reply)) != kIOReturnSuccess) { usleep(40000); continue; }
        // Reply after tag 0x02: [result][vcp][type][maxH][maxL][curH][curL]
        int tag = ddc_find_tag(reply, sizeof(reply), 0x02);
        if (tag >= 0 && tag + 6 < (int)sizeof(reply) && reply[tag + 2] == vcp) {
            if (max) *max = (reply[tag + 4] << 8) | reply[tag + 5];
            if (cur) *cur = (reply[tag + 6] << 8) | reply[tag + 7];
            return kIOReturnSuccess;
        }
        usleep(40000);
    }
    return kIOReturnNotFound;
}
*/
import "C"

import (
	"fmt"
	"strings"
	"unsafe"
)

// Display is one DDC-controllable external monitor.
type Display struct {
	Index int
	Name  string
	svc   C.IOAVServiceRef
}

// List enumerates external displays that expose an IOAVService (Apple Silicon).
func List() ([]*Display, error) {
	var out []*Display

	// Walk the IORegistry for DCPAVServiceProxy nodes whose Location is External.
	matching := C.IOServiceNameMatching(C.CString("DCPAVServiceProxy"))
	var iter C.io_iterator_t
	// kIOMainPortDefault == 0 (a.k.a. the old kIOMasterPortDefault).
	if C.IOServiceGetMatchingServices(0, C.CFDictionaryRef(matching), &iter) != C.kIOReturnSuccess {
		// Fall back to the single default service.
		if svc := C.IOAVServiceCreate(C.kCFAllocatorDefault); svc != 0 {
			out = append(out, &Display{Index: 0, Name: "Default display", svc: svc})
		}
		return out, nil
	}
	defer C.IOObjectRelease(iter)

	idx := 0
	locKey := cfstr("Location")
	defer C.CFRelease(C.CFTypeRef(locKey))
	for {
		service := C.IOIteratorNext(iter)
		if service == 0 {
			break
		}
		loc := C.IORegistryEntrySearchCFProperty(service, C.CString("IOService"), locKey, C.kCFAllocatorDefault, C.kIORegistryIterateRecursively|C.kIORegistryIterateParents)
		isExternal := false
		if loc != 0 {
			isExternal = cfStringEquals(C.CFStringRef(loc), "External")
			C.CFRelease(loc)
		}
		if isExternal {
			if svc := C.IOAVServiceCreateWithService(C.kCFAllocatorDefault, service); svc != 0 {
				out = append(out, &Display{
					Index: idx,
					Name:  registryName(service, idx),
					svc:   svc,
				})
				idx++
			}
		}
		C.IOObjectRelease(service)
	}

	if len(out) == 0 {
		if svc := C.IOAVServiceCreate(C.kCFAllocatorDefault); svc != 0 {
			out = append(out, &Display{Index: 0, Name: "Default display", svc: svc})
		}
	}
	return out, nil
}

// SetVCP writes an arbitrary VCP feature.
func (d *Display) SetVCP(vcp uint8, v uint16) error {
	if r := C.ddc_set(d.svc, C.uint8_t(vcp), C.uint16_t(v)); r != C.kIOReturnSuccess {
		return fmt.Errorf("ddc set 0x%02X=0x%04x failed: IOReturn 0x%x", vcp, v, uint32(r))
	}
	return nil
}

// GetVCP reads an arbitrary VCP feature (best-effort).
func (d *Display) GetVCP(vcp uint8) (cur, max uint16, err error) {
	var c, m C.uint16_t
	if r := C.ddc_get(d.svc, C.uint8_t(vcp), &c, &m); r != C.kIOReturnSuccess {
		return 0, 0, fmt.Errorf("ddc get 0x%02X failed: IOReturn 0x%x", vcp, uint32(r))
	}
	return uint16(c), uint16(m), nil
}

// SetInput switches the monitor's active input (VCP 0x60) to value v.
func (d *Display) SetInput(v uint16) error {
	return d.SetVCP(0x60, v)
}

// Capabilities returns the monitor's DDC/MCCS capabilities string (best-effort;
// many panels — and Apple Silicon DDC reads generally — are unreliable here).
func (d *Display) Capabilities() (string, error) {
	buf := make([]byte, 4096)
	n := C.ddc_caps(d.svc, (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)))
	if n <= 0 {
		return "", fmt.Errorf("capabilities read failed")
	}
	return string(buf[:n]), nil
}

// GetInput reads the current input source (VCP 0x60). Best-effort; some panels
// refuse DDC reads even though writes work.
func (d *Display) GetInput() (cur uint16, max uint16, err error) {
	return d.GetVCP(0x60)
}

// DebugRawGetVCP returns up to 12 raw reply bytes (diagnostic only).
func (d *Display) DebugRawGetVCP(vcp uint8) ([]byte, error) {
	out := make([]byte, 12)
	if r := C.ddc_get_raw(d.svc, C.uint8_t(vcp), (*C.uchar)(unsafe.Pointer(&out[0]))); r != C.kIOReturnSuccess {
		return nil, fmt.Errorf("raw get failed: IOReturn 0x%x", uint32(r))
	}
	return out, nil
}

// DebugRead writes a get-VCP(0x60) then reads with the given read offset.
func (d *Display) DebugRead(roffset uint32, n int) ([]byte, uint32) {
	out := make([]byte, n)
	r := C.ddc_read_dbg(d.svc, C.uint32_t(roffset), (*C.uchar)(unsafe.Pointer(&out[0])), C.int(n))
	return out, uint32(r)
}

// Close releases the underlying IOAVService.
func (d *Display) Close() {
	if d.svc != 0 {
		C.CFRelease(C.CFTypeRef(d.svc))
		d.svc = 0
	}
}

// SetInputByMatch switches the first display whose name contains match (case-
// insensitive). Empty match uses the first display.
func SetInputByMatch(match string, v uint16) error {
	displays, err := List()
	if err != nil {
		return err
	}
	if len(displays) == 0 {
		return fmt.Errorf("no DDC-capable external displays found")
	}
	defer func() {
		for _, d := range displays {
			d.Close()
		}
	}()
	for _, d := range displays {
		if match == "" || strings.Contains(strings.ToLower(d.Name), strings.ToLower(match)) {
			return d.SetInput(v)
		}
	}
	return fmt.Errorf("no display matched %q", match)
}

// --- small CoreFoundation helpers ----------------------------------------

func cfstr(s string) C.CFStringRef {
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	return C.CFStringCreateWithCString(C.kCFAllocatorDefault, cs, C.kCFStringEncodingUTF8)
}

func cfStringEquals(ref C.CFStringRef, want string) bool {
	if ref == 0 {
		return false
	}
	buf := make([]C.char, 256)
	if C.CFStringGetCString(ref, &buf[0], 256, C.kCFStringEncodingUTF8) == C.false {
		return false
	}
	return C.GoString(&buf[0]) == want
}

func registryName(service C.io_service_t, idx int) string {
	var name [128]C.char
	if C.IORegistryEntryGetName(service, &name[0]) == C.kIOReturnSuccess {
		if n := C.GoString(&name[0]); n != "" {
			return fmt.Sprintf("%s #%d", n, idx+1)
		}
	}
	return fmt.Sprintf("External display #%d", idx+1)
}
