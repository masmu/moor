package internal

import (
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/walles/moor/v2/internal/linemetadata"
	"github.com/walles/moor/v2/internal/reader"
)

// Scroll to the next search hit, while the user is typing the search string.
func (p *Pager) scrollToSearchHits() {
	if p.searchPattern == nil {
		// This is not a search
		return
	}

	lineIndex := p.scrollPosition.lineIndex(p)
	if lineIndex == nil {
		// No lines to search
		return
	}

	if p.searchHitIsVisible() {
		// Already on-screen
		return
	}

	if p.scrollRightToSearchHits() {
		// Found it to the right, done!
		return
	}

	firstHitIndex := p.findFirstHit(*lineIndex, nil, false)
	if firstHitIndex == nil {
		alreadyAtTheTop := (*lineIndex == linemetadata.Index{})
		if alreadyAtTheTop {
			// No match, can't wrap, give up
			return
		}

		// Try again from the top
		firstHitIndex = p.findFirstHit(linemetadata.Index{}, lineIndex, false)
	}
	if firstHitIndex == nil {
		// No match, give up
		return
	}

	// Found a match on some line
	p.scrollPosition = NewScrollPositionFromIndex(*firstHitIndex, "scrollToSearchHits")

	p.leftColumnZeroBased = 0
	p.showLineNumbers = p.ShowLineNumbers
	if !p.searchHitIsVisible() {
		p.scrollRightToSearchHits()
	}
}

// Scroll to the next search hit, when the user presses 'n'.
func (p *Pager) scrollToNextSearchHit() {
	if p.searchPattern == nil {
		// Nothing to search for, never mind
		return
	}

	if p.Reader().GetLineCount() == 0 {
		// Nothing to search in, never mind
		return
	}

	if p.scrollRightToSearchHits() {
		// Found it to the right, done!
		return
	}

	if p.isViewing() && p.isScrolledToEnd() {
		p.mode = PagerModeNotFound{pager: p}
		return
	}

	var firstSearchIndex linemetadata.Index

	switch {
	case p.isViewing():
		// Start searching on the first line below the bottom of the screen
		position := p.getLastVisiblePosition().NextLine(1)
		firstSearchIndex = *position.lineIndex(p)

	case p.isNotFound():
		// Restart searching from the top
		p.mode = PagerModeViewing{pager: p}
		firstSearchIndex = linemetadata.Index{}

	default:
		panic(fmt.Sprint("Unknown search mode when finding next: ", p.mode))
	}

	firstHitIndex := p.findFirstHit(firstSearchIndex, nil, false)
	if firstHitIndex == nil {
		p.mode = PagerModeNotFound{pager: p}
		return
	}
	p.scrollPosition = NewScrollPositionFromIndex(*firstHitIndex, "scrollToNextSearchHit")

	// Don't let any search hit scroll out of sight
	p.setTargetLine(nil)

	p.leftColumnZeroBased = 0
	p.showLineNumbers = p.ShowLineNumbers
	if !p.searchHitIsVisible() {
		p.scrollRightToSearchHits()
	}
}

// Scroll backwards to the previous search hit, while the user is typing the
// search string.
func (p *Pager) scrollToSearchHitsBackwards() {
	if p.searchPattern == nil {
		// This is not a search
		return
	}

	// Start at the bottom of the currently visible screen
	lastVisiblePosition := p.getLastVisiblePosition()
	if lastVisiblePosition == nil {
		// No lines to search
		return
	}
	lineIndex := lastVisiblePosition.lineIndex(p)
	if lineIndex == nil {
		log.Warn("No line number to search even though we have a last visible position")
		return
	}

	if p.searchHitIsVisible() {
		// Already on-screen
		return
	}

	if p.scrollLeftToSearchHits() {
		// Found it to the left, done!
		return
	}

	firstHitIndex := p.findFirstHit(*lineIndex, nil, true)
	if firstHitIndex == nil {
		lastLineIndex := linemetadata.IndexFromLength(p.Reader().GetLineCount())
		if lastLineIndex == nil {
			// In the first part of the search we had some lines to search.
			// Lines should never go away, so this should never happen.
			log.Error("Wrapped backwards search had no lines to search")
			return
		}
		canWrap := (*lineIndex != *lastLineIndex)
		if !canWrap {
			// No match, can't wrap, give up
			return
		}

		// Try again from the bottom
		firstHitIndex = p.findFirstHit(*lastLineIndex, lineIndex, true)
	}
	if firstHitIndex == nil {
		// No match, give up
		return
	}

	firstHitPosition := NewScrollPositionFromIndex(*firstHitIndex, "scrollToSearchHitsBackwards")

	// Scroll so that the first hit is at the bottom of the screen
	p.scrollPosition = firstHitPosition.PreviousLine(p.visibleHeight() - 1)

	p.scrollMaxRight()
	if !p.searchHitIsVisible() {
		p.scrollLeftToSearchHits()
	}
}

// Scroll backwards to the previous search hit, when the user presses 'N'.
func (p *Pager) scrollToPreviousSearchHit() {
	if p.searchPattern == nil {
		// Nothing to search for, never mind
		return
	}

	if p.Reader().GetLineCount() == 0 {
		// Nothing to search in, never mind
		return
	}

	var firstSearchIndex linemetadata.Index

	switch {
	case p.isViewing():
		// Start searching on the first line above the top of the screen
		position := p.scrollPosition.PreviousLine(1)
		firstSearchIndex = *position.lineIndex(p)

	case p.isNotFound():
		// Restart searching from the bottom
		p.mode = PagerModeViewing{pager: p}
		firstSearchIndex = *linemetadata.IndexFromLength(p.Reader().GetLineCount())

	default:
		panic(fmt.Sprint("Unknown search mode when finding previous: ", p.mode))
	}

	firstHitIndex := p.findFirstHit(firstSearchIndex, nil, true)
	if firstHitIndex == nil {
		p.mode = PagerModeNotFound{pager: p}
		return
	}
	p.scrollPosition = *scrollPositionFromIndex("scrollToPreviousSearchHit", *firstHitIndex)

	// Don't let any search hit scroll out of sight
	p.setTargetLine(nil)
}

// Search input lines. Not screen lines!
//
// The `beforePosition` parameter is exclusive, meaning that line will not be
// searched.
//
// For the actual searching, this method will call _findFirstHit() in parallel
// on multiple cores, to help large file search performance.
func (p *Pager) findFirstHit(startPosition linemetadata.Index, beforePosition *linemetadata.Index, backwards bool) *linemetadata.Index {
	// If the number of lines to search matches the number of cores (or more),
	// divide the search into chunks. Otherwise use one chunk.
	chunkCount := runtime.NumCPU()
	var linesCount int
	if backwards {
		// If the startPosition is zero, that should make the count one
		linesCount = startPosition.Index() + 1
		if beforePosition != nil {
			// Searching from 1 with before set to 0 should make the count 1
			linesCount = startPosition.Index() - beforePosition.Index()
		}
	} else {
		linesCount = p.Reader().GetLineCount() - startPosition.Index()
		if beforePosition != nil {
			// Searching from 1 with before set to 2 should make the count 1
			linesCount = beforePosition.Index() - startPosition.Index()
		}
	}

	if linesCount < chunkCount {
		chunkCount = 1
	}
	chunkSize := linesCount / chunkCount

	log.Debugf("Searching %d lines across %d cores with %d lines per core...", linesCount, chunkCount, chunkSize)
	t0 := time.Now()
	defer func() {
		linesPerSecond := float64(linesCount) / time.Since(t0).Seconds()
		linesPerSecondS := fmt.Sprintf("%.0f", linesPerSecond)
		if linesPerSecond > 7_000_000.0 {
			linesPerSecondS = fmt.Sprintf("%.0fM", linesPerSecond/1000_000.0)
		} else if linesPerSecond > 7_000.0 {
			linesPerSecondS = fmt.Sprintf("%.0fk", linesPerSecond/1000.0)
		}

		if linesCount > 0 {
			log.Debugf("Searched %d lines in %s at %slines/s or %s/line",
				linesCount,
				time.Since(t0),
				linesPerSecondS,
				time.Since(t0)/time.Duration(linesCount))
		} else {
			log.Debugf("Searched %d lines in %s at %slines/s", linesCount, time.Since(t0), linesPerSecondS)
		}
	}()

	// Each parallel search will start at one of these positions
	searchStarts := make([]linemetadata.Index, chunkCount)
	direction := 1
	if backwards {
		direction = -1
	}
	for i := 0; i < chunkCount; i++ {
		searchStarts[i] = startPosition.NonWrappingAdd(i * direction * chunkSize)
	}

	// Make a results array, with one result per chunk
	findings := make([]chan *linemetadata.Index, chunkCount)

	// Search all chunks in parallel
	for i, searchStart := range searchStarts {
		findings[i] = make(chan *linemetadata.Index)

		searchEndIndex := i + 1
		var chunkBefore *linemetadata.Index
		if searchEndIndex < len(searchStarts) {
			chunkBefore = &searchStarts[searchEndIndex]
		} else if beforePosition != nil {
			chunkBefore = beforePosition
		}

		reader := p.Reader()
		pattern := *p.searchPattern
		go func(i int, searchStart linemetadata.Index, chunkBefore *linemetadata.Index) {
			defer func() {
				PanicHandler("findFirstHit()/chunkSearch", recover(), debug.Stack())
			}()

			findings[i] <- _findFirstHit(reader, searchStart, pattern, chunkBefore, backwards)
		}(i, searchStart, chunkBefore)
	}

	// Return the first non-nil result
	for _, finding := range findings {
		result := <-finding
		if result != nil {
			return result
		}
	}

	return nil
}

// NOTE: When we search, we do that by looping over the *input lines*, not the
// screen lines. That's why startPosition is an Index rather than a
// scrollPosition.
//
// The `beforePosition` parameter is exclusive, meaning that line will not be
// searched.
//
// This method will run over multiple chunks of the input file in parallel to
// help large file search performance.
//
// FIXME: We should take startPosition.deltaScreenLines into account as well!
func _findFirstHit(reader reader.Reader, startPosition linemetadata.Index, pattern regexp.Regexp, beforePosition *linemetadata.Index, backwards bool) *linemetadata.Index {
	searchPosition := startPosition
	for {
		line := reader.GetLine(searchPosition)
		if line == nil {
			// No match, give up
			return nil
		}

		lineText := line.Plain()
		if pattern.MatchString(lineText) {
			return &searchPosition
		}

		if backwards {
			if (searchPosition == linemetadata.Index{}) {
				// Reached the top without any match, give up
				return nil
			}

			searchPosition = searchPosition.NonWrappingAdd(-1)
		} else {
			searchPosition = searchPosition.NonWrappingAdd(1)
		}

		if beforePosition != nil && searchPosition == *beforePosition {
			// No match, give up
			return nil
		}
	}
}

// Return true if any search hit is currently visible on screen.
//
// A search hit is considered visible if the first character of the hit is
// visible. This means that if the hit is longer than one character, the rest of
// it may be off-screen to the right. If that happens, the user can scroll right
// manually to see the rest of the hit.
func (p *Pager) searchHitIsVisible() bool {
	rendered := p.renderLines()
	contentLines := rendered.lines
	if len(contentLines) == 0 {
		// No lines on screen, no hits
		return false
	}

	for _, row := range contentLines {
		for _, cell := range row.cells {
			if cell.StartsSearchHit {
				// Found a search hit on screen!
				return true
			}
		}
	}

	// No search hits found
	return false
}

// Scroll right looking for search hits. Return true if we found any.
func (p *Pager) scrollRightToSearchHits() bool {
	if p.WrapLongLines {
		// No horizontal scrolling when wrapping
		return false
	}

	// Check how far right we can scroll at most. Factors involved:
	// - Screen width
	// - Length of longest visible line
	screenWidth, _ := p.screen.Size()

	longestLineLength := 0 // In screen cells, some runes are double-width
	rendered := p.renderLines()
	for _, inputLine := range rendered.inputLines {
		lineLength := inputLine.DisplayWidth()
		if lineLength > longestLineLength {
			longestLineLength = lineLength
		}
	}

	// With a 10 wide screen and a 15 wide line (max index 14), the leftmost
	// screen column can at most be 5:
	//
	// Screen column: 0123456789
	// Line column:   5678901234
	maxLeftmostColumn := longestLineLength - screenWidth

	restoreShowLineNumbers := p.showLineNumbers
	restoreLeftColumn := p.leftColumnZeroBased

	for p.leftColumnZeroBased < maxLeftmostColumn {
		// FIXME: Rather than scrolling right one screen at a time, we should
		// consider scanning all lines for search hits and scrolling directly to the
		// first one that is off-screen to the right.

		// If the screen width is 1, and we have no line numbers, the answer
		// could be 1. But since the last column could be covered by scroll-right
		// markers, we'll say 0.
		firstNotVisibleColumn := p.leftColumnZeroBased + screenWidth - rendered.numberPrefixWidth - 1
		if firstNotVisibleColumn < 0 {
			log.Info("Screen is narrower than number prefix length, not scrolling right for search hits")
			p.showLineNumbers = restoreShowLineNumbers
			p.leftColumnZeroBased = restoreLeftColumn
			return false
		}

		scrollToColumn := firstNotVisibleColumn
		if scrollToColumn > maxLeftmostColumn {
			scrollToColumn = maxLeftmostColumn
		}

		p.showLineNumbers = false
		p.leftColumnZeroBased = scrollToColumn

		if p.searchHitIsVisible() {
			// Found it!
			return true
		}
	}

	// Can't scroll right, pretend nothing happened
	p.showLineNumbers = restoreShowLineNumbers
	p.leftColumnZeroBased = restoreLeftColumn
	return false
}

// Scroll left looking for search hits. Return true if we found any.
func (p *Pager) scrollLeftToSearchHits() bool {
	if p.WrapLongLines {
		// No horizontal scrolling when wrapping
		return false
	}

	restoreLeftColumn := p.leftColumnZeroBased
	restoreShowLineNumbers := p.showLineNumbers

	screenWidth, _ := p.screen.Size()

	// If we go max left, which column will be the rightmost visible one?
	var fullLeftRightmostVisibleColumn int
	{
		p.showLineNumbers = p.ShowLineNumbers
		p.leftColumnZeroBased = 0
		rendered := p.renderLines()
		// If the screen width is 2, we have columns 0 and 1. The rightmost column can be covered by
		// scroll-right markers, so the first not-visible column when fully scrolled left is 0, or
		// "2 - 2".
		fullLeftRightmostVisibleColumn = screenWidth - 2 - rendered.numberPrefixWidth

		p.leftColumnZeroBased = restoreLeftColumn
		p.showLineNumbers = restoreShowLineNumbers
	}

	// FIXME: Handle fullLeftRightmostVisibleColumn <= 0?

	// Keep scrolling left until we either find a search hit, or reach the
	// leftmost column with line numbers shown or not based on the user's
	// preference.
	for p.leftColumnZeroBased > 0 || (p.showLineNumbers != p.ShowLineNumbers) {
		// FIXME: Rather than scrolling left one screen at a time, we should
		// consider scanning all lines for search hits and scrolling directly to the
		// first one that is off-screen to the left.

		// Pretend the current leftmost column is not visible, since it could be
		// covered by scroll-left markers.
		lastNotVisibleColumn := p.leftColumnZeroBased

		// Go left
		if lastNotVisibleColumn <= fullLeftRightmostVisibleColumn {
			// Going max left will show the column we want
			p.showLineNumbers = p.ShowLineNumbers
			p.leftColumnZeroBased = 0
		} else {
			// Scroll left one screen.
			//
			// If the screen width is 3, and we want column 5 to be visible, we
			// should be showing columns 3, 4, and 5. So we want to set the
			// leftmost column to 3, which is "5 - 3 + 1"
			scrollToColumn := lastNotVisibleColumn - screenWidth + 1
			if scrollToColumn < 0 {
				scrollToColumn = 0
			}

			p.leftColumnZeroBased = scrollToColumn

			// If showing line numbers was possible we should have ended up in
			// the other if branch ^
			p.showLineNumbers = false
		}

		if p.searchHitIsVisible() {
			// Found it!
			return true
		}
	}

	// Scrolling left didn't find anything, pretend nothing happened
	p.showLineNumbers = restoreShowLineNumbers
	p.leftColumnZeroBased = restoreLeftColumn
	return false
}

func (p *Pager) isViewing() bool {
	_, isViewing := p.mode.(PagerModeViewing)
	return isViewing
}

func (p *Pager) isNotFound() bool {
	_, isNotFound := p.mode.(PagerModeNotFound)
	return isNotFound
}
