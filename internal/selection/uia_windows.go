//go:build windows

package selection

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
)

// The UI Automation path: read the focused element or a containing
// element that exposes its TextPattern selection. Zero side effects — nothing touches the clipboard — and it
// names the selection's rectangle, which the clipboard path cannot.
//
// A frozen target app blocks the COM call, so every grab runs on a fresh
// MTA thread with a timeout that abandons it: a stuck call then costs
// one goroutine, not the app. The calls also go one at a time — two
// concurrent calls into the same provider, a timed-out grab's abandoned
// call and the next grab's fresh one, can hang each other — and
// consecutive timeouts open a circuit breaker that skips UIA for a
// while, so a bad neighbor does not add 800ms to every grab.

const (
	// Chromium UIA answers slowly on its first call, when the accessibility
	// tree is still warming up; 600ms timed out on grabs that would land.
	uiaTimeout         = 800 * time.Millisecond
	uiaBreakerAfter    = 3
	uiaBreakerCooldown = 60 * time.Second
	// How long a grab queues for the UIA slot before giving up on it. A
	// short wait rides out the normal overlap — the previous call just
	// finishing — but a call stuck inside the provider holds the slot
	// for good, and the clipboard beats queueing behind it.
	uiaBusyWait = 150 * time.Millisecond
)

// The internal outcomes grabUIA reports to Grab.
var (
	errUIAMiss           = errors.New("uia: no text pattern or selection")
	errUIASkipped        = errors.New("uia: skipped (breaker open or a call in flight)")
	errUIATimeout        = errors.New("uia: timeout")
	errUIAPatternMissing = fmt.Errorf("uia: text pattern unavailable: %w", errUIAMiss)
	errUIAEmpty          = fmt.Errorf("uia: no selection ranges: %w", errUIAMiss)
)

// Vtable slots, counted from UIAutomationClient.h's interface orders
// (IUnknown's three first).
const (
	vtblRelease                    = 2
	vtblUiaGetFocusedElement       = 8
	vtblUiaGetRawViewWalker        = 16
	vtblWalkerGetParentElement     = 3
	vtblElementGetCurrentPatternAs = 14
	vtblElementGetProcessID        = 20
	vtblElementGetControlType      = 21
	vtblElementGetIsPassword       = 35
	vtblTextPatternGetSelection    = 5
	vtblRangeArrayGetLength        = 3
	vtblRangeArrayGetElement       = 4
	vtblRangeGetBoundingRectangles = 10
	vtblRangeGetText               = 12
)

const (
	uiaTextPatternID       = 10014
	uiaWindowControlTypeID = 50032
)

var (
	clsidCUIAutomation = ole.NewGUID("{ff48dba4-60ef-4201-aa87-54103eef594e}")
	iidIUIAutomation   = ole.NewGUID("{30cbe57d-d9d0-452a-ab13-7ac5ac4825ee}")
	iidITextPattern    = ole.NewGUID("{32eba289-3583-42c9-9c59-3b6d9a1e9b6a}")
)

// uiaBreaker skips UIA for a while after consecutive timeouts.
type uiaBreaker struct {
	mu        sync.Mutex
	timeouts  int
	openUntil time.Time
}

var breaker uiaBreaker

// uiaMu admits one UIA call at a time; acquiring is a receive, like
// clipboardMu. The worker holds the slot until its COM call returns —
// even one the grab has already timed out on — so a stuck call keeps
// the slot and the next grab skips UIA instead of piling a second call
// onto the same provider. A call that never returns therefore disables
// UIA for the rest of the process; the clipboard path carries on.
var uiaMu = make(chan struct{}, 1)

// Correlate a late worker return with its timed-out caller without logging
// element names, selection text, or other provider content.
var uiaCallID atomic.Uint64

func init() { uiaMu <- struct{}{} }

func (b *uiaBreaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().After(b.openUntil)
}

func (b *uiaBreaker) noteTimeout() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.timeouts++
	if b.timeouts >= uiaBreakerAfter {
		b.openUntil = time.Now().Add(uiaBreakerCooldown)
		b.timeouts = 0
		log.Printf("selection: uia breaker=open cooldown=%s", uiaBreakerCooldown)
	}
}

func (b *uiaBreaker) noteAnswer() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.timeouts = 0
}

func grabUIA(ctx context.Context) (Selection, error) {
	hwnd, pid, _ := foregroundWindow()
	sel, err := grabUIAFrom(ctx, pid)
	if !sameForeground(hwnd, pid) {
		return Selection{}, fmt.Errorf("foreground after UIA: %w", ErrSourceChanged)
	}
	return sel, err
}

func grabUIAFrom(ctx context.Context, pid uint32) (Selection, error) {
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	return grabUIAWith(ctx, func(selected func(string, string)) (Selection, error) {
		return uiaGrabThread(workerCtx, pid, selected)
	})
}

func grabUIAWith(ctx context.Context, read func(func(string, string)) (Selection, error)) (sel Selection, err error) {
	started, outcome, hit := time.Now(), "canceled", "none"
	callID := uiaCallID.Add(1)
	defer func() {
		elapsed := time.Since(started).Round(time.Millisecond)
		switch {
		case outcome == "selected":
			logSelection(ctx, "uia hit=%s elapsed=%s", hit, elapsed)
		case outcome == "selected_without_bounds" || outcome == "timeout":
			logSelection(ctx, "uia call=%d outcome=%s hit=%s elapsed=%s", callID, outcome, hit, elapsed)
		case outcome == "error":
			logSelection(ctx, "uia err=%v elapsed=%s", err, elapsed)
		default:
			logSelection(ctx, "uia outcome=%s elapsed=%s", outcome, elapsed)
		}
	}()
	if err := ctx.Err(); err != nil {
		return Selection{}, err
	}
	if !breaker.allow() {
		outcome = "breaker_open"
		return Selection{}, errUIASkipped
	}
	select {
	case <-ctx.Done():
		return Selection{}, ctx.Err()
	case <-uiaMu:
	case <-time.After(uiaBusyWait):
		// The slot is taken by a call still inside the provider —
		// exactly the call a timeout has already abandoned once.
		outcome = "slot_busy"
		return Selection{}, errUIASkipped
	}
	if err := ctx.Err(); err != nil {
		uiaMu <- struct{}{}
		return Selection{}, err
	}
	type answer struct {
		sel Selection
		err error
	}
	ch := make(chan answer, 1)
	type nativeText struct{ text, hit string }
	var selected atomic.Pointer[nativeText]
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer func() { uiaMu <- struct{}{} }()
		started := time.Now()
		sel, err := read(func(text, hit string) { selected.Store(&nativeText{text, hit}) })
		// Keep a separate worker line only for calls that outlast the UIA budget.
		if elapsed := time.Since(started); elapsed >= uiaTimeout {
			logSelection(ctx, "event=native_worker backend=uia call=%d outcome=returned elapsed=%s err=%v", callID, elapsed.Round(time.Millisecond), err)
		}
		ch <- answer{sel, err}
	}()
	timer := time.NewTimer(uiaTimeout)
	defer timer.Stop()
	select {
	case a := <-ch:
		breaker.noteAnswer()
		switch {
		case errors.Is(a.err, errUIAPatternMissing):
			outcome = "pattern_missing"
		case errors.Is(a.err, errUIAEmpty):
			outcome = "empty"
		case errors.Is(a.err, errProtectedSelection):
			outcome = "protected"
		case errors.Is(a.err, errUIAMiss):
			outcome = "miss"
		case a.err != nil:
			outcome = "error"
		case a.sel.Text == "":
			outcome = "no_text"
		default:
			outcome = "selected"
			if result := selected.Load(); result != nil {
				hit = result.hit
			}
		}
		return a.sel, a.err
	case <-timer.C:
		// Bounds and COM cleanup can stall after GetText already succeeded.
		// Keep the text while the worker retains its slot until it returns.
		if text := selected.Load(); text != nil {
			breaker.noteAnswer()
			outcome = "selected_without_bounds"
			hit = text.hit
			return Selection{Text: text.text, Via: "uia"}, nil
		}
		breaker.noteTimeout()
		outcome = "timeout"
		return Selection{}, errUIATimeout
	case <-ctx.Done():
		return Selection{}, ctx.Err()
	}
}

// uiaGrabThread runs on its own thread: a UIA client must join the MTA,
// and a fresh thread per grab keeps a hung call from blocking the next.
func uiaGrabThread(ctx context.Context, pid uint32, selected func(string, string)) (Selection, error) {
	// COINIT_MULTITHREADED = 0: the apartment UIA clients want, which
	// needs no message pump.
	if err := ole.CoInitializeEx(0, 0); err != nil {
		return Selection{}, fmt.Errorf("uia: CoInitializeEx: %w", err)
	}
	defer ole.CoUninitialize()

	unk, err := ole.CreateInstance(clsidCUIAutomation, iidIUIAutomation)
	if err != nil {
		return Selection{}, fmt.Errorf("uia: CoCreateInstance(CUIAutomation): %w", err)
	}
	auto := unsafe.Pointer(unk)
	defer comCall(auto, vtblRelease)

	var elem unsafe.Pointer
	if err := comCall(auto, vtblUiaGetFocusedElement, uintptr(unsafe.Pointer(&elem))); err != nil {
		return Selection{}, fmt.Errorf("uia: GetFocusedElement: %w", err)
	}
	if elem == nil {
		return Selection{}, errUIAMiss
	}
	defer comCall(elem, vtblRelease)
	hit := "focus"
	read := func(ctx context.Context, node unsafe.Pointer) (Selection, error) {
		if err := ctx.Err(); err != nil {
			return Selection{}, err
		}
		return uiaElementSelection(node, func(text string) { selected(text, hit) })
	}
	sel, err := read(ctx, elem)
	if err != errTrySelectionParent {
		return sel, err
	}
	if ctx.Err() != nil || !uiaOwnedBy(elem, pid) {
		return Selection{}, errUIAMiss
	}

	// Chromium can focus a leaf while its containing document exposes the
	// selection. The raw walker follows that branch, including non-controls.
	var walker unsafe.Pointer
	if err := comCall(auto, vtblUiaGetRawViewWalker, uintptr(unsafe.Pointer(&walker))); err != nil || walker == nil {
		return Selection{}, errUIAMiss
	}
	defer comCall(walker, vtblRelease)
	hit = "ancestor"
	sel, err = readSelectionParents(ctx, elem, 48, read, func(node unsafe.Pointer) unsafe.Pointer {
		return uiaSelectionParent(walker, node, pid)
	}, func(node unsafe.Pointer) {
		comCall(node, vtblRelease)
	})
	if err == errTrySelectionParent {
		err = errUIAPatternMissing
	}
	return sel, err
}

func uiaOwnedBy(elem unsafe.Pointer, pid uint32) bool {
	var owner int32
	return pid != 0 && comCall(elem, vtblElementGetProcessID, uintptr(unsafe.Pointer(&owner))) == nil && uint32(owner) == pid
}

func uiaSelectionParent(walker, node unsafe.Pointer, pid uint32) unsafe.Pointer {
	var parent unsafe.Pointer
	if err := comCall(walker, vtblWalkerGetParentElement, uintptr(node), uintptr(unsafe.Pointer(&parent))); err != nil || parent == nil {
		return nil
	}
	var role int32
	if uiaOwnedBy(parent, pid) &&
		comCall(parent, vtblElementGetControlType, uintptr(unsafe.Pointer(&role))) == nil && role != uiaWindowControlTypeID {
		return parent
	}
	comCall(parent, vtblRelease)
	return nil
}

func uiaElementSelection(elem unsafe.Pointer, selected func(string)) (Selection, error) {
	var password int32
	if err := comCall(elem, vtblElementGetIsPassword, uintptr(unsafe.Pointer(&password))); err != nil {
		return Selection{}, errUIAMiss
	}
	if password != 0 {
		return Selection{}, errProtectedSelection
	}

	var pattern unsafe.Pointer
	if err := comCall(elem, vtblElementGetCurrentPatternAs,
		uintptr(uiaTextPatternID), uintptr(unsafe.Pointer(iidITextPattern)),
		uintptr(unsafe.Pointer(&pattern))); err != nil || pattern == nil {
		return Selection{}, errTrySelectionParent
	}
	defer comCall(pattern, vtblRelease)

	var ranges unsafe.Pointer
	if err := comCall(pattern, vtblTextPatternGetSelection, uintptr(unsafe.Pointer(&ranges))); err != nil || ranges == nil {
		return Selection{}, errUIAMiss
	}
	defer comCall(ranges, vtblRelease)

	var length int32
	if err := comCall(ranges, vtblRangeArrayGetLength, uintptr(unsafe.Pointer(&length))); err != nil {
		return Selection{}, errUIAMiss
	}
	if length == 0 {
		return Selection{}, errUIAEmpty
	}

	sel := Selection{Via: "uia"}
	for i := int32(0); i < length && sel.Text == ""; i++ {
		var r unsafe.Pointer
		if err := comCall(ranges, vtblRangeArrayGetElement, uintptr(uint32(i)), uintptr(unsafe.Pointer(&r))); err != nil || r == nil {
			continue
		}
		text, ok := rangeText(r)
		if ok && strings.TrimSpace(text) != "" {
			selected(text)
			sel.Text = text
			sel.Rect, sel.HasRect = rangeFirstRect(r)
		}
		comCall(r, vtblRelease)
	}
	if sel.Text == "" {
		// The pattern answered with selection ranges that hold no text.
		// Most apps mean "nothing selected", but some Chromium ones answer
		// this way while a selection exists, so grab decides whether the
		// simulated copy is worth trying.
		return Selection{}, nil
	}
	return sel, nil
}

// rangeText reads an ITextRange's text, or reports that it has none.
func rangeText(r unsafe.Pointer) (string, bool) {
	var bstr uintptr
	if err := comCall(r, vtblRangeGetText, ^uintptr(0), uintptr(unsafe.Pointer(&bstr))); err != nil || bstr == 0 {
		return "", false
	}
	text := bstrText(bstr)
	ole.SysFreeString((*int16)(native(bstr)))
	return text, text != ""
}

// Only the first rectangle anchors the bubble, even for a multiline selection.
func rangeFirstRect(r unsafe.Pointer) (Rect, bool) {
	var sa unsafe.Pointer
	if err := comCall(r, vtblRangeGetBoundingRectangles, uintptr(unsafe.Pointer(&sa))); err != nil || sa == nil {
		return Rect{}, false
	}
	defer procSafeArrayDestroy.Call(uintptr(sa))
	var lb, ub int32
	if hr, _, _ := procSafeArrayGetLBound.Call(uintptr(sa), 1, uintptr(unsafe.Pointer(&lb))); hr != 0 {
		return Rect{}, false
	}
	if hr, _, _ := procSafeArrayGetUBound.Call(uintptr(sa), 1, uintptr(unsafe.Pointer(&ub))); hr != 0 {
		return Rect{}, false
	}
	if int64(ub)-int64(lb) < 3 {
		return Rect{}, false
	}
	var coords [4]float64
	for i := range coords {
		if hr, _, _ := procSafeArrayGetElement.Call(uintptr(sa), uintptr(unsafe.Pointer(&lb)), uintptr(unsafe.Pointer(&coords[i]))); hr != 0 {
			return Rect{}, false
		}
		lb++
	}
	return physRectToDIP(coords[0], coords[1], coords[2], coords[3])
}

// bstrText reads a BSTR as a Go string.
func bstrText(p uintptr) string {
	n := ole.SysStringLen((*int16)(native(p)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(unsafe.Slice((*uint16)(native(p)), n))
}

// native turns a pointer Win32 returned into one Go can look through.
// The round trip through its own address keeps the conversion legal for
// go vet, which cannot know the uintptr held a pointer all along; the
// same trick mygo's own win32 layer uses.
func native(addr uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&addr))
}

// COM reads and writes through pointers passed as uintptr. Keep their Go
// storage alive and at a stable address through the indirect SyscallN.
//
//go:uintptrescapes
func comCall(obj unsafe.Pointer, slot int, args ...uintptr) error {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, slot*8))
	call := append([]uintptr{uintptr(obj)}, args...)
	hr, _, _ := syscall.SyscallN(fn, call...)
	if hr != 0 {
		return fmt.Errorf("HRESULT 0x%08x", int32(hr))
	}
	return nil
}
