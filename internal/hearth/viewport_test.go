package hearth

import "testing"

func TestScrollViewport_ScrollDown(t *testing.T) {
	vp := scrollViewport{}
	vp.ScrollDown(5)
	if vp.cursor != 1 {
		t.Errorf("cursor = %d, want 1", vp.cursor)
	}
	// Scroll to end
	for i := 0; i < 10; i++ {
		vp.ScrollDown(5)
	}
	if vp.cursor != 4 {
		t.Errorf("cursor = %d, want 4 (clamped to total-1)", vp.cursor)
	}
}

func TestScrollViewport_ScrollUp(t *testing.T) {
	vp := scrollViewport{cursor: 3}
	vp.ScrollUp()
	if vp.cursor != 2 {
		t.Errorf("cursor = %d, want 2", vp.cursor)
	}
	vp.cursor = 0
	vp.ScrollUp()
	if vp.cursor != 0 {
		t.Errorf("cursor = %d, want 0 (clamped)", vp.cursor)
	}
}

func TestScrollViewport_ClampToTotal(t *testing.T) {
	vp := scrollViewport{cursor: 5, viewStart: 3}
	vp.ClampToTotal(3)
	if vp.cursor != 2 {
		t.Errorf("cursor = %d, want 2", vp.cursor)
	}

	vp.ClampToTotal(0)
	if vp.cursor != 0 || vp.viewStart != 0 {
		t.Errorf("cursor=%d viewStart=%d, want both 0", vp.cursor, vp.viewStart)
	}
}

func TestScrollViewport_AdjustViewport_CursorAbove(t *testing.T) {
	vp := scrollViewport{cursor: 1, viewStart: 3}
	vp.AdjustViewport(5, 10)
	if vp.viewStart != 1 {
		t.Errorf("viewStart = %d, want 1 (scrolled up to cursor)", vp.viewStart)
	}
}

func TestScrollViewport_AdjustViewport_CursorBelow(t *testing.T) {
	vp := scrollViewport{cursor: 8, viewStart: 0}
	vp.AdjustViewport(5, 10)
	if vp.viewStart != 4 {
		t.Errorf("viewStart = %d, want 4 (scrolled down to show cursor)", vp.viewStart)
	}
}

func TestScrollViewport_AdjustViewport_AllFit(t *testing.T) {
	vp := scrollViewport{cursor: 2, viewStart: 1}
	vp.AdjustViewport(10, 5)
	if vp.viewStart != 0 {
		t.Errorf("viewStart = %d, want 0 (everything fits)", vp.viewStart)
	}
}

func TestScrollViewport_AdjustViewport_ClampMaxStart(t *testing.T) {
	vp := scrollViewport{cursor: 9, viewStart: 9}
	vp.AdjustViewport(5, 10)
	// maxStart = 10-5 = 5, cursor=9 means viewStart = 9-5+1 = 5
	if vp.viewStart != 5 {
		t.Errorf("viewStart = %d, want 5", vp.viewStart)
	}
}

func TestScrollViewport_VisibleRange(t *testing.T) {
	vp := scrollViewport{viewStart: 3}
	start, end := vp.VisibleRange(5, 10)
	if start != 3 || end != 8 {
		t.Errorf("range = [%d,%d), want [3,8)", start, end)
	}

	// Near end of list
	vp.viewStart = 8
	start, end = vp.VisibleRange(5, 10)
	if start != 8 || end != 10 {
		t.Errorf("range = [%d,%d), want [8,10)", start, end)
	}
}

func TestScrollViewport_FullCycle(t *testing.T) {
	// Simulate scrolling through a 10-item list with viewport of 3
	vp := scrollViewport{}
	total := 10
	viewHeight := 3

	// Scroll down to item 5
	for i := 0; i < 5; i++ {
		vp.ScrollDown(total)
	}
	if vp.cursor != 5 {
		t.Fatalf("cursor = %d, want 5", vp.cursor)
	}

	vp.AdjustViewport(viewHeight, total)
	start, end := vp.VisibleRange(viewHeight, total)

	// Cursor 5 should be visible in [3,6)
	if start > 5 || end <= 5 {
		t.Errorf("cursor 5 not in visible range [%d,%d)", start, end)
	}

	// Scroll back up to 0
	for i := 0; i < 10; i++ {
		vp.ScrollUp()
	}
	if vp.cursor != 0 {
		t.Fatalf("cursor = %d, want 0", vp.cursor)
	}

	vp.AdjustViewport(viewHeight, total)
	start, _ = vp.VisibleRange(viewHeight, total)
	if start != 0 {
		t.Errorf("viewStart = %d, want 0 after scrolling to top", start)
	}
}
