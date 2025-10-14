package internal

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/walles/moor/v2/internal/linemetadata"
	"github.com/walles/moor/v2/internal/reader"
	"github.com/walles/moor/v2/internal/textstyles"
	"github.com/walles/moor/v2/twin"
	"gotest.tools/v3/assert"
)

// NOTE: You can find related tests in pager_test.go.

// Converts a cell row to a plain string and removes trailing whitespace.
func renderedToString(row []textstyles.CellWithMetadata) string {
	rowString := ""
	for _, cell := range row {
		rowString += string(cell.Rune)
	}

	return strings.TrimRight(rowString, " ")
}

func testHorizontalCropping(t *testing.T, contents string, firstVisibleColumn int, lastVisibleColumn int, expected string) {
	pager := NewPager(nil)
	pager.ShowLineNumbers = false
	pager.showLineNumbers = false

	pager.screen = twin.NewFakeScreen(1+lastVisibleColumn-firstVisibleColumn, 99)
	pager.leftColumnZeroBased = firstVisibleColumn
	pager.scrollPosition = newScrollPosition("testHorizontalCropping")

	lineContents := reader.NewLine(contents)
	numberedLine := reader.NumberedLine{
		Line: &lineContents,
	}
	screenLine := pager.renderLine(&numberedLine, pager.getLineNumberPrefixLength(numberedLine.Number))
	assert.Equal(t, renderedToString(screenLine[0].cells), expected)
}

func TestCreateScreenLine(t *testing.T) {
	testHorizontalCropping(t, "abc", 0, 10, "abc")
}

func TestCreateScreenLineCanScrollLeft(t *testing.T) {
	testHorizontalCropping(t, "abc", 1, 10, "<c")
}

func TestCreateScreenLineCanScrollRight(t *testing.T) {
	testHorizontalCropping(t, "abc", 0, 1, "a>")
}

func TestCreateScreenLineCanAlmostScrollRight(t *testing.T) {
	testHorizontalCropping(t, "abc", 0, 2, "abc")
}

func TestCreateScreenLineCanScrollBoth(t *testing.T) {
	testHorizontalCropping(t, "abcde", 1, 3, "<c>")
}

func TestCreateScreenLineCanAlmostScrollBoth(t *testing.T) {
	testHorizontalCropping(t, "abcd", 1, 3, "<cd")
}

func TestCreateScreenLineChopWideCharLeft(t *testing.T) {
	testHorizontalCropping(t, "上午下", 0, 10, "上午下")
	testHorizontalCropping(t, "上午下", 1, 10, "<午下")
	testHorizontalCropping(t, "上午下", 2, 10, "< 下")
	testHorizontalCropping(t, "上午下", 3, 10, "<下")
	testHorizontalCropping(t, "上午下", 4, 10, "<")
	testHorizontalCropping(t, "上午下", 5, 10, "<")
	testHorizontalCropping(t, "上午下", 6, 10, "<")
	testHorizontalCropping(t, "上午下", 7, 10, "<")
}

func TestCreateScreenLineChopWideCharRight(t *testing.T) {
	testHorizontalCropping(t, "上午下", 0, 6, "上午下")
	testHorizontalCropping(t, "上午下", 0, 5, "上午下")
	testHorizontalCropping(t, "上午下", 0, 4, "上午>")
	testHorizontalCropping(t, "上午下", 0, 3, "上 >")
	testHorizontalCropping(t, "上午下", 0, 2, "上>")
	testHorizontalCropping(t, "上午下", 0, 1, " >")
}

func TestEmpty(t *testing.T) {
	pager := Pager{
		screen: twin.NewFakeScreen(99, 10),

		// No lines available
		readers: []*reader.ReaderImpl{reader.NewFromTextForTesting("test", "")},

		scrollPosition: newScrollPosition("TestEmpty"),
	}
	pager.filteringReader = FilteringReader{
		BackingReader: pager.readers[pager.currentReader],
		FilterPattern: &pager.filterPattern,
	}

	rendered := pager.renderLines()
	assert.Equal(t, len(rendered.lines), 0)
	assert.Equal(t, "test: <empty>", rendered.statusText)
	assert.Assert(t, pager.lineIndex() == nil)
}

// Repro case for a search bug discovered in v1.9.8.
func TestSearchHighlight(t *testing.T) {
	line := reader.NewLine("x\"\"x")
	pager := Pager{
		screen:        twin.NewFakeScreen(100, 10),
		searchPattern: regexp.MustCompile("\""),
	}
	// FIXME: Try removing this line and see if the test still passes. The
	// filtering reader was set up with empty values anyway, and I'm not sure
	// whether it's needed at all.
	pager.filteringReader = FilteringReader{}

	numberedLine := reader.NumberedLine{
		Line: &line,
	}
	rendered := pager.renderLine(&numberedLine, pager.getLineNumberPrefixLength(numberedLine.Number))
	assert.DeepEqual(t, []renderedLine{
		{
			inputLineIndex: linemetadata.Index{},
			wrapIndex:      0,
			cells: []textstyles.CellWithMetadata{
				{Rune: 'x', Style: twin.StyleDefault},
				{Rune: '"', Style: twin.StyleDefault.WithAttr(twin.AttrReverse), StartsSearchHit: true},
				{Rune: '"', Style: twin.StyleDefault.WithAttr(twin.AttrReverse), StartsSearchHit: false},
				{Rune: 'x', Style: twin.StyleDefault},
			},
		},
	}, rendered,
		cmp.AllowUnexported(twin.Style{}),
		cmp.AllowUnexported(renderedLine{}),
		cmp.AllowUnexported(linemetadata.Number{}),
		cmp.AllowUnexported(linemetadata.Index{}),
	)
}

func TestOverflowDown(t *testing.T) {
	pager := Pager{
		screen: twin.NewFakeScreen(
			10, // Longer than the raw line, we're testing vertical overflow, not horizontal
			2,  // Single line of contents + one status line
		),

		// Single line of input
		readers: []*reader.ReaderImpl{reader.NewFromTextForTesting("test", "hej")},

		// This value can be anything and should be clipped, that's what we're testing
		scrollPosition: *scrollPositionFromIndex("TestOverflowDown", linemetadata.IndexFromOneBased(42)),
	}
	pager.filteringReader = FilteringReader{
		BackingReader: pager.readers[pager.currentReader],
		FilterPattern: &pager.filterPattern,
	}

	rendered := pager.renderLines()
	assert.Equal(t, len(rendered.lines), 1)
	assert.Equal(t, "hej", renderedToString(rendered.lines[0].cells))
	assert.Equal(t, "test: 1 line  100%", rendered.statusText)
	assert.Assert(t, pager.lineIndex().IsZero())
	assert.Equal(t, pager.deltaScreenLines(), 0)
}

func TestOverflowUp(t *testing.T) {
	pager := Pager{
		screen: twin.NewFakeScreen(
			10, // Longer than the raw line, we're testing vertical overflow, not horizontal
			2,  // Single line of contents + one status line
		),

		// Single line of input
		readers: []*reader.ReaderImpl{reader.NewFromTextForTesting("test", "hej")},

		// NOTE: scrollPosition intentionally not initialized
	}
	pager.filteringReader = FilteringReader{
		BackingReader: pager.readers[pager.currentReader],
		FilterPattern: &pager.filterPattern,
	}

	rendered := pager.renderLines()
	assert.Equal(t, len(rendered.lines), 1)
	assert.Equal(t, "hej", renderedToString(rendered.lines[0].cells))
	assert.Equal(t, "test: 1 line  100%", rendered.statusText)
	assert.Assert(t, pager.lineIndex().IsZero())
	assert.Equal(t, pager.deltaScreenLines(), 0)
}

func TestWrapping(t *testing.T) {
	reader := reader.NewFromTextForTesting("",
		"first line\nline two will be wrapped\nhere's the last line")
	pager := NewPager(reader)
	pager.screen = twin.NewFakeScreen(40, 40)

	pager.WrapLongLines = true
	pager.ShowLineNumbers = false
	pager.showLineNumbers = false

	assert.NilError(t, reader.Wait())

	// This is what we're testing really
	pager.scrollToEnd()

	// Higher than needed, we'll just be validating the necessary lines at the
	// top.
	screen := twin.NewFakeScreen(10, 99)

	// Exit immediately
	pager.Quit()

	// Get contents onto our fake screen
	pager.StartPaging(screen, nil, nil)
	pager.redraw("")

	actual := strings.Join([]string{
		rowToString(screen.GetRow(0)),
		rowToString(screen.GetRow(1)),
		rowToString(screen.GetRow(2)),
		rowToString(screen.GetRow(3)),
		rowToString(screen.GetRow(4)),
		rowToString(screen.GetRow(5)),
		rowToString(screen.GetRow(6)),
		rowToString(screen.GetRow(7)),
	}, "\n")
	assert.Equal(t, actual, strings.Join([]string{
		"first line",
		"line two",
		"will be",
		"wrapped",
		"here's the",
		"last line",
		"---",
		"",
	}, "\n"))
}

// Repro for https://github.com/walles/moor/issues/153
func TestOneLineTerminal(t *testing.T) {
	pager := Pager{
		// Single line terminal window, this is what we're testing
		screen: twin.NewFakeScreen(20, 1),

		readers:       []*reader.ReaderImpl{reader.NewFromTextForTesting("test", "hej")},
		ShowStatusBar: true,
	}
	pager.filteringReader = FilteringReader{
		BackingReader: pager.readers[pager.currentReader],
		FilterPattern: &pager.filterPattern,
	}

	rendered := pager.renderLines()
	assert.Equal(t, len(rendered.lines), 0)
}

// What happens if we are scrolled to the bottom of a 1000 lines file, and then
// add a filter matching only the first line?
//
// What should happen is that we should go as far down as possible.
func TestShortenedInput(t *testing.T) {
	pager := Pager{
		screen: twin.NewFakeScreen(20, 10),

		// 1000 lines of input, we will scroll to the bottom
		readers: []*reader.ReaderImpl{reader.NewFromTextForTesting("test", "first\n"+strings.Repeat("line\n", 1000))},

		scrollPosition: newScrollPosition("TestShortenedInput"),
	}

	// Hide the status bar for this test
	pager.mode = PagerModeViewing{&pager}
	pager.ShowStatusBar = false

	pager.filteringReader = FilteringReader{
		BackingReader: pager.readers[pager.currentReader],
		FilterPattern: &pager.filterPattern,
	}

	pager.scrollToEnd()
	assert.Equal(t, pager.lineIndex().Index(), 991, "This should have been the effect of calling scrollToEnd()")

	pager.mode = NewPagerModeFilter(&pager)
	pager.filterPattern = regexp.MustCompile("first") // Match only the first line

	rendered := pager.renderLines()
	assert.Equal(t, len(rendered.lines), 1, "Should have rendered one line")
	assert.Equal(t, "first", renderedToString(rendered.lines[0].cells))
	assert.Equal(t, pager.lineIndex().Index(), 0, "Should have scrolled to the first line")
}

// - Start with a 1000 lines file
// - Scroll to the bottom
// - Add a filter matching the first 100 lines
// - Render
// - Verify that the 10 last matching lines were rendered
func TestShortenedInputManyLines(t *testing.T) {
	lines := []string{"first"}
	for i := range 999 {
		if i < 100 {
			lines = append(lines, "match "+strconv.Itoa(i))
		} else {
			lines = append(lines, "other "+strconv.Itoa(i))
		}
	}

	pager := Pager{
		screen:         twin.NewFakeScreen(20, 10),
		readers:        []*reader.ReaderImpl{reader.NewFromTextForTesting("test", strings.Join(lines, "\n"))},
		scrollPosition: newScrollPosition("TestShortenedInputManyLines"),
	}

	pager.filteringReader = FilteringReader{
		BackingReader: pager.readers[pager.currentReader],
		FilterPattern: &pager.filterPattern,
	}

	pager.scrollToEnd()
	assert.Equal(t, pager.lineIndex().Index(), 991, "Should be at the last line before filtering")

	pager.mode = NewPagerModeFilter(&pager)
	pager.filterPattern = regexp.MustCompile(`^match`)

	rendered := pager.renderLines()
	assert.Equal(t, len(rendered.lines), 9, "Should have rendered 9 lines (10 minus one status bar)")

	expectedLines := []string{}
	for i := 91; i < 100; i++ {
		expectedLines = append(expectedLines, "match "+strconv.Itoa(i))
	}
	for i, row := range rendered.lines {
		assert.Equal(t, renderedToString(row.cells), expectedLines[i], "Line %d mismatch", i)
	}
	assert.Equal(t, pager.lineIndex().Index(), 91, "The last lines should now be visible")
	assert.Equal(t, "match 99", renderedToString(rendered.lines[len(rendered.lines)-1].cells))
}

func BenchmarkRenderLines(b *testing.B) {
	input := reader.NewFromTextForTesting(
		"BenchmarkRenderLine()",
		strings.Repeat("This is a line with text and some more text to make it long enough.\n", 100))
	pager := NewPager(input)
	pager.screen = twin.NewFakeScreen(80, 25)

	assert.NilError(b, input.Wait())

	pager.renderLines() // Warm up

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pager.renderLines()
	}
}
