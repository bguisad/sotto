package selection

import (
	"context"
	"errors"
	"testing"
)

func TestReadSelectionParentsFindsDocument(t *testing.T) {
	// A focused Chromium leaf can omit selected text that its document exposes.
	const focus, document = 1, 25
	live := map[int]bool{focus: true}
	want := Selection{Text: "selected in document", Rect: Rect{X: 10, Y: 20, W: 30, H: 15}, HasRect: true}
	got, err := readSelectionParents(t.Context(), focus, document-focus, func(_ context.Context, node int) (Selection, error) {
		if len(live) != 2 || !live[focus] || !live[node] {
			t.Fatalf("unexpected live elements while reading %d: %v", node, live)
		}
		if node == document {
			return want, nil
		}
		return Selection{}, errTrySelectionParent
	}, func(node int) int {
		if !live[node] || node == document {
			t.Fatalf("unexpected parent request on %d", node)
		}
		live[node+1] = true
		return node + 1
	}, func(node int) { delete(live, node) })
	if err != nil || got != want || len(live) != 1 || !live[focus] {
		t.Fatalf("selection=%+v err=%v live=%v", got, err, live)
	}
}

func TestReadSelectionParentsStopsAtProtectedElement(t *testing.T) {
	// An enclosing document must not supply text once a protected control is found.
	_, err := readSelectionParents(t.Context(), 1, 4, func(_ context.Context, node int) (Selection, error) {
		if node != 2 {
			t.Fatalf("read past protected element: %d", node)
		}
		return Selection{}, errProtectedSelection
	}, func(node int) int { return node + 1 }, func(int) {})
	if !errors.Is(err, errProtectedSelection) {
		t.Fatalf("expected protected selection, got %v", err)
	}
}

func TestReadSelectionParentsStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := readSelectionParents(ctx, 1, 4, func(_ context.Context, node int) (Selection, error) {
		t.Fatalf("read after cancellation: %d", node)
		return Selection{}, nil
	}, func(node int) int {
		cancel()
		return node + 1
	}, func(int) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}
