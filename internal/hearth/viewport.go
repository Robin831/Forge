package hearth

// scrollViewport manages a selection cursor with a separate viewport offset
// for scrollable list panels. The cursor tracks which item is selected; the
// viewStart tracks the first visible item in the viewport. The viewport
// adjusts automatically to keep the cursor visible without jumping.
type scrollViewport struct {
	cursor    int // selected item index
	viewStart int // first visible item index
}

// ScrollDown moves the cursor down by one, clamped to total-1.
func (v *scrollViewport) ScrollDown(total int) {
	if v.cursor < total-1 {
		v.cursor++
	}
}

// ScrollUp moves the cursor up by one, clamped to 0.
func (v *scrollViewport) ScrollUp() {
	if v.cursor > 0 {
		v.cursor--
	}
}

// ClampToTotal adjusts cursor and viewStart when the backing list shrinks
// (e.g. after a data refresh). Call this whenever the total item count changes.
func (v *scrollViewport) ClampToTotal(total int) {
	if total == 0 {
		v.cursor = 0
		v.viewStart = 0
		return
	}
	if v.cursor >= total {
		v.cursor = total - 1
	}
	if v.viewStart >= total {
		v.viewStart = total - 1
	}
}

// AdjustViewport ensures the cursor is visible within a viewport of the given
// height (in item slots, not lines). Call this during rendering when the
// viewport height is known.
func (v *scrollViewport) AdjustViewport(viewHeight, total int) {
	if viewHeight <= 0 {
		viewHeight = 1
	}
	// Scroll viewport up if cursor is above the visible area.
	if v.cursor < v.viewStart {
		v.viewStart = v.cursor
	}
	// Scroll viewport down if cursor is below the visible area.
	if v.cursor >= v.viewStart+viewHeight {
		v.viewStart = v.cursor - viewHeight + 1
	}
	// Clamp viewStart to valid range.
	if v.viewStart < 0 {
		v.viewStart = 0
	}
	if total <= viewHeight {
		v.viewStart = 0
	} else {
		maxStart := total - viewHeight
		if v.viewStart > maxStart {
			v.viewStart = maxStart
		}
	}
}

// VisibleRange returns the start (inclusive) and end (exclusive) indices of
// items visible in a viewport of the given height. Call AdjustViewport first.
func (v *scrollViewport) VisibleRange(viewHeight, total int) (start, end int) {
	if viewHeight <= 0 {
		viewHeight = 1
	}
	start = v.viewStart
	end = start + viewHeight
	if end > total {
		end = total
	}
	if start > total {
		start = total
	}
	return start, end
}

// Selected returns the current cursor position.
func (v *scrollViewport) Selected() int {
	return v.cursor
}
