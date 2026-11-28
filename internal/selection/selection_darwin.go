//go:build darwin

package selection

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego/objc"
	"github.com/egoist/mygo"

	"sotto/internal/hotkey"
	"sotto/internal/macnative"
)

var (
	axApplication          func(int32) uintptr
	axAttribute            func(uintptr, objc.ID, *uintptr) int32
	axParameter            func(uintptr, objc.ID, uintptr, *uintptr) int32
	axTimeout              func(uintptr, float32) int32
	axPID                  func(uintptr, *int32) int32
	axValue                func(uintptr, uint32, unsafe.Pointer) bool
	axValueType            func(uintptr) uint32
	axElementID, axValueID func() uint64
	cfRelease              func(uintptr)
	cfTypeID               func(uintptr) uint64
	cfStringID             func() uint64
)

var loadAX = sync.OnceValue(func() error {
	if _, err := macnative.Framework("AppKit"); err != nil {
		return err
	}
	if err := macnative.Bind("CoreFoundation", macnative.Symbol{Target: &cfRelease, Name: "CFRelease"}, macnative.Symbol{Target: &cfTypeID, Name: "CFGetTypeID"}, macnative.Symbol{Target: &cfStringID, Name: "CFStringGetTypeID"}); err != nil {
		return err
	}
	return macnative.Bind("ApplicationServices",
		macnative.Symbol{Target: &axElementID, Name: "AXUIElementGetTypeID"}, macnative.Symbol{Target: &axValueID, Name: "AXValueGetTypeID"},
		macnative.Symbol{Target: &axApplication, Name: "AXUIElementCreateApplication"},
		macnative.Symbol{Target: &axAttribute, Name: "AXUIElementCopyAttributeValue"},
		macnative.Symbol{Target: &axParameter, Name: "AXUIElementCopyParameterizedAttributeValue"},
		macnative.Symbol{Target: &axTimeout, Name: "AXUIElementSetMessagingTimeout"},
		macnative.Symbol{Target: &axPID, Name: "AXUIElementGetPid"},
		macnative.Symbol{Target: &axValue, Name: "AXValueGetValue"},
		macnative.Symbol{Target: &axValueType, Name: "AXValueGetType"})
})

type macSource struct {
	pid        int32
	generation uint64
	self       bool
}

func prepareMacSource() (s macSource) {
	s.pid, s.generation = macnative.FrontmostPID(), macnative.WorkspaceGeneration()
	s.self = s.pid == int32(os.Getpid())
	// A nonactivating panel may be key while the external app is frontmost.
	// Prepare runs on the UI thread before hiding our panels.
	mygo.RunOnMain(func() {
		macnative.Pool(func() {
			app := macnative.Class("NSApplication").Send(macnative.Sel("sharedApplication"))
			s.self = s.self || app.Send(macnative.Sel("keyWindow")) != 0
		})
	})
	return s
}

func (s macSource) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.self {
		return ErrSelfWindow
	}
	if s.pid <= 0 || s.generation != macnative.WorkspaceGeneration() || s.pid != macnative.FrontmostPID() {
		return ErrSourceChanged
	}
	return nil
}

func Prepare(shortcut string) func(context.Context) (Selection, error) {
	s := prepareMacSource()
	mainKey := hotkey.MacMainKeyCode(shortcut)
	return func(ctx context.Context) (Selection, error) {
		logSelection(ctx, "event=source pid=%d self=%t", s.pid, s.self)
		return readMacSelection(ctx, s.check, macnative.RequireAccessibility, func(ctx context.Context) (Selection, error) { return readAX(ctx, s) }, func(ctx context.Context) (Selection, error) { return copyMacSelection(ctx, s, mainKey) })
	}
}

func readMacSelection(ctx context.Context, check func(context.Context) error, permission func(bool) error, read, copy func(context.Context) (Selection, error)) (Selection, error) {
	if err := check(ctx); err != nil {
		return Selection{}, err
	}
	if err := permission(true); err != nil {
		return Selection{}, err
	}
	sel, err := read(ctx)
	if err == nil && sel.Text != "" {
		if err := check(ctx); err != nil {
			return Selection{}, err
		}
		return sel, nil
	}
	if err != nil && !errors.Is(err, ErrNoSelection) {
		return Selection{}, err
	}
	if err := check(ctx); err != nil {
		return Selection{}, err
	}
	logSelection(ctx, "fallback=clipboard")
	return copy(ctx)
}

func grab(ctx context.Context) (Selection, error) { return Prepare("")(ctx) }
func grabUIA(ctx context.Context) (Selection, error) {
	s := prepareMacSource()
	if err := macnative.RequireAccessibility(true); err != nil {
		return Selection{}, err
	}
	return readAX(ctx, s)
}
func grabClipboard(ctx context.Context) (Selection, error) {
	s := prepareMacSource()
	if err := macnative.RequireAccessibility(true); err != nil {
		return Selection{}, err
	}
	return copyMacSelection(ctx, s, -1)
}

const (
	axSelectionTimeout     = 250 * time.Millisecond
	axAttributeUnsupported = -25205
	axNoValue              = -25212
)

var axAdmission = make(chan struct{}, 1)

func readAX(ctx context.Context, s macSource) (sel Selection, err error) {
	started, outcome, hit := time.Now(), "miss", "focus"
	defer func() {
		switch {
		case err == nil:
			logSelection(ctx, "ax hit=%s elapsed=%s", hit, time.Since(started).Round(time.Millisecond))
		case errors.Is(err, ErrNoSelection):
			logSelection(ctx, "ax outcome=%s elapsed=%s", outcome, time.Since(started).Round(time.Millisecond))
		default:
			logSelection(ctx, "ax err=%v elapsed=%s", err, time.Since(started).Round(time.Millisecond))
		}
	}()
	if err := loadAX(); err != nil {
		return sel, err
	}
	if err := s.check(ctx); err != nil {
		return sel, err
	}
	select {
	case axAdmission <- struct{}{}:
	case <-ctx.Done():
		return sel, ctx.Err()
	}
	defer func() { <-axAdmission }()
	if err := s.check(ctx); err != nil {
		return sel, err
	}
	macnative.Pool(func() {
		app := axApplication(s.pid)
		if app == 0 {
			err = ErrNoSelection
			return
		}
		defer cfRelease(app)
		if err = setAXSelectionTimeout(ctx, app); err != nil {
			return
		}
		var focus uintptr
		if axAttribute(app, macnative.String("AXFocusedUIElement"), &focus) != 0 || focus == 0 {
			err = ErrNoSelection
			return
		}
		defer cfRelease(focus)
		if cfTypeID(focus) != axElementID() {
			err = ErrNoSelection
			return
		}
		if err = setAXSelectionTimeout(ctx, focus); err != nil {
			return
		}
		read := func(ctx context.Context, node uintptr) (Selection, error) {
			return readAXElementSelection(ctx, node, &outcome)
		}
		sel, err = read(ctx, focus)
		if err != errTrySelectionParent {
			return
		}
		// Give the added traversal its own budget before falling back to copy.
		walkCtx, cancel := context.WithTimeout(ctx, axSelectionTimeout)
		defer cancel()
		hit = "ancestor"
		sel, err = readSelectionParents(walkCtx, focus, 16, read, func(node uintptr) uintptr {
			return axSelectionParent(walkCtx, node, s.pid)
		}, cfRelease)
		if err == context.DeadlineExceeded || walkCtx.Err() == context.DeadlineExceeded && err == errTrySelectionParent {
			outcome, err = "timeout", ErrNoSelection
		} else if err == errTrySelectionParent {
			err = ErrNoSelection
		}
	})
	if sourceErr := s.check(ctx); sourceErr != nil {
		return Selection{}, sourceErr
	}
	return sel, err
}

// AX messaging timeouts belong to individual element references.
func setAXSelectionTimeout(ctx context.Context, node uintptr) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timeout := axSelectionTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	if timeout <= 0 {
		return context.DeadlineExceeded
	}
	if code := axTimeout(node, float32(timeout.Seconds())); code != 0 {
		return fmt.Errorf("AX messaging timeout: %d", code)
	}
	return nil
}

// A focused link may omit selected text that its containing document exposes.
func axSelectionParent(ctx context.Context, node uintptr, pid int32) uintptr {
	var parent uintptr
	if axAttribute(node, macnative.String("AXParent"), &parent) != 0 || parent == 0 {
		return 0
	}
	var owner int32
	if cfTypeID(parent) == axElementID() && axPID(parent, &owner) == 0 && owner == pid && setAXSelectionTimeout(ctx, parent) == nil {
		// Window and application nodes can aggregate unrelated selections.
		role, _ := axStringAttribute(parent, "AXRole")
		if role != "" && role != "AXWindow" && role != "AXApplication" {
			return parent
		}
	}
	cfRelease(parent)
	return 0
}

func axStringAttribute(node uintptr, attribute string) (string, int32) {
	var value uintptr
	code := axAttribute(node, macnative.String(attribute), &value)
	if code != 0 || value == 0 {
		return "", code
	}
	defer cfRelease(value)
	if cfTypeID(value) != cfStringID() {
		return "", code
	}
	return macnative.GoString(objc.ID(value)), code
}

func readAXElementSelection(ctx context.Context, node uintptr, outcome *string) (Selection, error) {
	if err := ctx.Err(); err != nil {
		return Selection{}, err
	}
	if subrole, _ := axStringAttribute(node, "AXSubrole"); subrole == "AXSecureTextField" {
		return Selection{}, errProtectedSelection
	}
	if err := ctx.Err(); err != nil {
		return Selection{}, err
	}
	text, code := axStringAttribute(node, "AXSelectedText")
	if err := ctx.Err(); err != nil {
		return Selection{}, err
	}
	switch code {
	case axAttributeUnsupported, axNoValue:
		*outcome = "attribute_missing"
		return Selection{}, errTrySelectionParent
	case 0:
		// An explicit empty selection can be an insertion point. An enclosing
		// element may still expose another selection, so keep copy as fallback.
		if text != "" {
			sel := Selection{Text: text, Via: "ax"}
			sel.Rect, sel.HasRect = axSelectionBounds(node)
			return sel, nil
		}
		*outcome = "empty"
	default:
		*outcome = "miss"
	}
	return Selection{}, ErrNoSelection
}

func axSelectionBounds(node uintptr) (Rect, bool) {
	var span uintptr
	if axAttribute(node, macnative.String("AXSelectedTextRange"), &span) != 0 || span == 0 {
		return Rect{}, false
	}
	defer cfRelease(span)
	if cfTypeID(span) != axValueID() || axValueType(span) != 4 {
		return Rect{}, false
	}
	var bounds uintptr
	if axParameter(node, macnative.String("AXBoundsForRange"), span, &bounds) != 0 || bounds == 0 {
		return Rect{}, false
	}
	defer cfRelease(bounds)
	var r macnative.Rect
	if cfTypeID(bounds) == axValueID() && axValueType(bounds) == 3 && axValue(bounds, 3, unsafe.Pointer(&r)) &&
		r.Size.Width > 0 && r.Size.Height > 0 && math.Abs(r.Origin.X) <= 1<<30 && math.Abs(r.Origin.Y) <= 1<<30 &&
		r.Size.Width <= 1<<30 && r.Size.Height <= 1<<30 {
		return Rect{int(math.Round(r.Origin.X)), int(math.Round(r.Origin.Y)), int(math.Round(r.Size.Width)), int(math.Round(r.Size.Height))}, true
	}
	return Rect{}, false
}

const macControlModifier = 1 << 18

// CapturePrimaryPress is sampled on the UI thread before deferring a blur.
func CapturePrimaryPress() PrimaryPress {
	events := macnative.Class("NSEvent")
	if events.Send(macnative.Sel("pressedMouseButtons"))&1 != 0 &&
		events.Send(macnative.Sel("modifierFlags"))&macControlModifier == 0 {
		return PrimaryPress{At: time.Now(), Point: mygo.Screen.CursorScreenPoint()}
	}
	return PrimaryPress{}
}

func WatchWindowLeave(handle uintptr, changed func(PrimaryPress)) (stop func(), err error) {
	var global, local, activation func()
	mygo.RunOnMain(func() {
		const leftMouseDown = 1
		const mask uint64 = 1<<leftMouseDown | 1<<3 | 1<<25
		notify := func(event objc.ID) {
			var press PrimaryPress
			if event.Send(macnative.Sel("type")) == leftMouseDown &&
				event.Send(macnative.Sel("modifierFlags"))&macControlModifier == 0 {
				press = PrimaryPress{At: time.Now(), Point: mygo.Screen.CursorScreenPoint()}
			}
			go changed(press)
		}
		global, err = macnative.Monitor(mask, false, notify)
		if err != nil {
			return
		}
		local, err = macnative.Monitor(mask, true, func(event objc.ID) {
			hit := event.Send(macnative.Sel("window"))
			if hit != 0 && !macnative.MenuWindow(hit) && !macnative.WindowContains(objc.ID(handle), hit) {
				notify(event)
			}
		})
		if err != nil {
			global()
			return
		}
		activation, err = macnative.WatchActivation(func() { go changed(PrimaryPress{}) })
		if err != nil {
			global()
			local()
		}
	})
	if err != nil {
		return nil, err
	}
	return sync.OnceFunc(func() { mygo.RunOnMain(func() { global(); local(); activation() }) }), nil
}

func WindowOwnsForeground(handle uintptr) bool {
	var owns bool
	mygo.RunOnMain(func() {
		app := macnative.Class("NSApplication").Send(macnative.Sel("sharedApplication"))
		key := app.Send(macnative.Sel("keyWindow"))
		owns = macnative.WindowContains(objc.ID(handle), key) || macnative.MenuWindow(key)
	})
	return owns
}
