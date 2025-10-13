// Package twin provides Terminal Window Interaction
package twin

import (
	"fmt"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
	"golang.org/x/term"
)

type MouseMode int

const (
	MouseModeAuto MouseMode = iota

	// Don't capture mouse events. This makes selecting with the mouse work. On
	// some terminals mouse scrolling will work using arrow keys emulation, and
	// on some not.
	MouseModeSelect

	// Capture mouse events. This makes mouse scrolling work. Special gymnastics
	// will be required for marking with the mouse to copy text.
	MouseModeScroll
)

type Screen interface {
	// Close() restores terminal to normal state, must be called after you are
	// done with your screen
	Close()

	Clear()

	// Returns the width of the rune just added, in number of columns.
	//
	// Note that if you set a wide rune (like '午') in one column, then whatever
	// you put in the next column will be hidden by the wide rune. A wide rune
	// in the last screen column will be replaced by a space, to prevent it from
	// overflowing onto the next line.
	SetCell(column int, row int, styledRune StyledRune) int

	// Render our contents into the terminal window
	Show()

	// Can be called after Close()ing the screen to fake retaining its output.
	// Plain Show() is what you'd call during normal operation.
	ShowNLines(lineCountToShow int)

	// Returns screen width and height.
	//
	// NOTE: Never cache this response! On window resizes you'll get an
	// EventResize on the Screen.Events channel, and this method will start
	// returning the new size instead.
	Size() (width int, height int)

	// ShowCursorAt() moves the cursor to the given screen position and makes
	// sure it is visible.
	//
	// If the position is outside of the screen, the cursor will be hidden.
	ShowCursorAt(column int, row int)

	// Can be nil if not (yet?) detected
	TerminalBackground() *Color

	// This channel is what your main loop should be checking.
	Events() chan Event
}

type interruptableReader interface {
	Read(p []byte) (n int, err error)

	// Interrupt unblocks the read call, either now or eventually.
	Interrupt()
}

type UnixScreen struct {
	widthAccessFromSizeOnly  int // Access from Size() method only
	heightAccessFromSizeOnly int // Access from Size() method only

	terminalBackground      *Color
	terminalBackgroundQuery *time.Time // When we asked for the terminal background color
	terminalBackgroundLock  sync.Mutex

	cells [][]StyledRune

	// Note that the type here doesn't matter, we only want to know whether or
	// not this channel has been signalled
	sigwinch chan int

	events chan Event

	ttyInReader interruptableReader

	ttyIn            *os.File
	oldTerminalState *term.State //nolint Not used on Windows
	oldTtyInMode     uint32      //nolint Windows only

	ttyOut        *os.File
	oldTtyOutMode uint32 //nolint Windows only

	terminalColorCount ColorCount
}

// Example event: "\x1b[<65;127;41M"
//
// Where:
//   - "\x1b[<" says this is a mouse event
//   - "65" says this is Wheel Up. "64" would be Wheel Down.
//   - "127" is the column number on screen, "1" is the first column.
//   - "41" is the row number on screen, "1" is the first row.
//   - "M" marks the end of the mouse event.
var mouseEventRegex = regexp.MustCompile("^\x1b\\[<([0-9]+);([0-9]+);([0-9]+)M")

// NewScreen() requires Close() to be called after you are done with your new
// screen, most likely somewhere in your shutdown code.
func NewScreen() (Screen, error) {
	return NewScreenWithMouseMode(MouseModeAuto)
}

func NewScreenWithMouseMode(mouseMode MouseMode) (Screen, error) {
	terminalColorCount := ColorCount24bit
	if os.Getenv("COLORTERM") != "truecolor" && strings.Contains(os.Getenv("TERM"), "256") {
		// Covers "xterm-256color" as used by the macOS Terminal
		terminalColorCount = ColorCount256
	}
	return NewScreenWithMouseModeAndColorCount(mouseMode, terminalColorCount)
}

func NewScreenWithMouseModeAndColorCount(mouseMode MouseMode, terminalColorCount ColorCount) (Screen, error) {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil, fmt.Errorf("stdout (fd=%d) must be a terminal for paging to work", os.Stdout.Fd())
	}

	screen := UnixScreen{
		terminalColorCount: terminalColorCount,
	}

	// The number "80" here is from manual testing on my MacBook:
	//
	// First, start "./moor.sh sample-files/large-git-log-patch.txt".
	//
	// Then do a two finger flick initiating a momentum based scroll-up.
	//
	// Now, if you get "Events buffer full" warnings, the buffer is too small.
	//
	// By this definition, 40 was too small, and 80 was OK.
	//
	// Bumped to 160 because of: https://github.com/walles/moor/issues/164
	screen.events = make(chan Event, 160)

	screen.setupSigwinchNotification()
	err := screen.setupTtyInTtyOut()
	if err != nil {
		return nil, fmt.Errorf("problem setting up TTY: %w", err)
	}
	screen.ttyInReader, err = newInterruptableReader(screen.ttyIn)
	if err != nil {
		restoreErr := screen.restoreTtyInTtyOut()
		if restoreErr != nil {
			log.Warn("Problem restoring TTY state after failed interruptable reader setup: ", restoreErr)
		}
		return nil, fmt.Errorf("problem setting up TTY reader: %w", err)
	}

	screen.setAlternateScreenMode(true)

	if mouseMode == MouseModeAuto {
		screen.enableMouseTracking(!terminalHasArrowKeysEmulation())
	} else if mouseMode == MouseModeSelect {
		screen.enableMouseTracking(false)
	} else if mouseMode == MouseModeScroll {
		screen.enableMouseTracking(true)
	} else {
		panic(fmt.Errorf("unknown mouse mode: %d", mouseMode))
	}

	screen.hideCursor(true)

	go func() {
		defer func() {
			panicHandler("NewScreenWithMouseModeAndColorCount()/mainLoop()", recover(), debug.Stack())
		}()

		screen.mainLoop()
	}()

	// Request terminal background color. The response will be handled in
	// screen.mainLoop() that we just started ^.
	//
	// Ref:
	// https://stackoverflow.com/questions/2507337/how-to-determine-a-terminals-background-color
	fmt.Println("\x1b]11;?\x07")
	screen.terminalBackgroundLock.Lock()
	defer screen.terminalBackgroundLock.Unlock()
	now := time.Now()
	screen.terminalBackgroundQuery = &now

	return &screen, nil
}

// Close() restores terminal to normal state, must be called after you are done
// with the screen returned by NewScreen()
func (screen *UnixScreen) Close() {
	// Tell the pager to exit unless it hasn't already
	screen.events <- EventExit{}

	// Tell our main loop to exit
	screen.ttyInReader.Interrupt()

	screen.hideCursor(false)
	screen.enableMouseTracking(false)
	screen.setAlternateScreenMode(false)

	err := screen.restoreTtyInTtyOut()
	if err != nil {
		// Debug logging because this is expected to fail in some cases:
		// * https://github.com/walles/moor/issues/145
		// * https://github.com/walles/moor/issues/149
		// * https://github.com/walles/moor/issues/150
		log.Info("Problem restoring TTY state: ", err)
	}
}

func (screen *UnixScreen) Events() chan Event {
	return screen.events
}

// Write string to ttyOut, panic on failure, return number of bytes written.
func (screen *UnixScreen) write(s string) int {
	bytesWritten, err := screen.ttyOut.Write([]byte(s))
	if err != nil {
		panic(err)
	}
	return bytesWritten
}

func (screen *UnixScreen) setAlternateScreenMode(enable bool) {
	// Ref: https://stackoverflow.com/a/11024208/473672
	if enable {
		screen.write("\x1b[?1049h")

		// Enable alternateScroll mode. This makes the mouse wheel work without
		// blocking selection.
		//
		// Ref: https://github.com/walles/moor/issues/53#issuecomment-3392572761
		screen.write("\x1b[?1007h")
	} else {
		screen.write("\x1b[?1007l")
		screen.write("\x1b[?1049l")
	}
}

func (screen *UnixScreen) hideCursor(hide bool) {
	// Ref: https://en.wikipedia.org/wiki/ANSI_escape_code#CSI_(Control_Sequence_Introducer)_sequences
	if hide {
		screen.write("\x1b[?25l")
	} else {
		screen.write("\x1b[?25h")
	}
}

// Tell both screen.Size() and the client app that the window was resized
func (screen *UnixScreen) onWindowResized() {
	select {
	case screen.sigwinch <- 0:
		// Screen.Size() method notified about resize
	default:
		// Notification already pending, never mind
	}

	// Notify client app.
	select {
	case screen.events <- EventResize{}:
		// Event delivered
	default:
		// This likely means that the user isn't processing events
		// quickly enough. Maybe the user's queue will get flooded if
		// the window is resized too quickly?
		log.Warn("Unable to deliver EventResize, event queue full")
	}
}

// Some terminals convert mouse events to key events making scrolling better
// without our built-in mouse support, and some do not.
//
// For those that do, we're better off without mouse tracking.
//
// To test your terminal, run with `moor --mousemode=mark` and see if mouse
// scrolling still works (both down and then back up to the top). If it does,
// add another check to this function!
//
// See also: https://github.com/walles/moor/issues/53
func terminalHasArrowKeysEmulation() bool {
	// Better off with mouse tracking:
	// * Terminal.app (macOS)
	// * Contour, thanks to @postsolar (GitHub username) for testing, 2023-12-18
	// * Foot, thanks to @postsolar (GitHub username) for testing, 2023-12-19

	// Hyper, tested on macOS, December 14th 2023
	if os.Getenv("TERM_PROGRAM") == "Hyper" {
		log.Info("Hyper terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Kitty, tested on macOS, December 14th 2023
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		log.Info("Kitty terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Alacritty, tested on macOS, December 14th 2023
	if os.Getenv("ALACRITTY_WINDOW_ID") != "" {
		log.Info("Alacritty terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Warp, tested on macOS, December 14th 2023
	if os.Getenv("TERM_PROGRAM") == "WarpTerminal" {
		log.Info("Warp terminal detected, assuming arrow keys emulation active")
		return true
	}

	// GNOME Terminal, tested on Ubuntu 22.04, December 16th 2023
	if os.Getenv("GNOME_TERMINAL_SCREEN") != "" {
		log.Info("GNOME Terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Tilix, tested on Ubuntu 22.04, December 16th 2023
	if os.Getenv("TILIX_ID") != "" {
		log.Info("Tilix terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Konsole, tested on Ubuntu 22.04, December 16th 2023
	if os.Getenv("KONSOLE_VERSION") != "" {
		log.Info("Konsole terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Terminator, tested on Ubuntu 22.04, December 16th 2023
	if os.Getenv("TERMINATOR_UUID") != "" {
		log.Info("Terminator terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Foot, tested on Ubuntu 22.04, December 16th 2023
	if os.Getenv("TERM") == "foot" || strings.HasPrefix(os.Getenv("TERM"), "foot-") {
		// Note that this test isn't very good, somebody could be running Foot
		// with some other TERM setting. Other suggestions welcome.
		log.Info("Foot terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Wezterm, tested on MacOS 12.6, January 3rd, 2024
	if os.Getenv("TERM_PROGRAM") == "WezTerm" {
		log.Info("Wezterm terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Rio, tested on macOS 14.3, January 27th, 2024
	if os.Getenv("TERM_PROGRAM") == "rio" {
		log.Info("Rio terminal detected, assuming arrow keys emulation active")
		return true
	}

	// VSCode 1.89.0, tested on macOS 14.4, May 6th, 2024
	if os.Getenv("TERM_PROGRAM") == "vscode" {
		log.Info("VSCode terminal detected, assuming arrow keys emulation active")
		return true
	}

	// IntelliJ IDEA CE 2023.2.2, tested on macOS 14.4, May 6th, 2024
	if os.Getenv("TERM_PROGRAM") == "JetBrains-JediTerm" {
		log.Info("IntelliJ IDEA terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Ghostty 1.0.1, tested on macOS 15.1.1, Jan 12th, 2025
	if os.Getenv("TERM_PROGRAM") == "ghostty" {
		log.Info("Ghostty terminal detected, assuming arrow keys emulation active")
		return true
	}

	// Windows Terminal, tested here:
	// https://github.com/walles/moor/issues/53#issuecomment-3276404279
	if os.Getenv("WT_SESSION") != "" {
		log.Info("Windows Terminal detected, assuming arrow keys emulation active")
		return true
	}

	// iTerm2, supports alternateScroll mode, and therefore works with "select"
	if os.Getenv("TERM_PROGRAM") == "iTerm.app" {
		log.Info("iTerm2 terminal detected, gets arrow keys emulation through alternateScroll mode")
		return true
	}

	// In ssh sessions we can't detect the terminal, but most terminals support
	// "select" mode, especially now that we activate alternateScroll as well.
	// Go for select.
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_TTY") != "" {
		log.Info("SSH session detected, assuming arrow keys emulation active since most terminals support it, especially with alternateScroll mode active")
		return true
	}

	log.Info("No known terminal with arrow keys emulation detected, assuming mouse tracking is needed")
	return false
}

func (screen *UnixScreen) enableMouseTracking(enable bool) {
	if enable {
		screen.write("\x1b[?1006;1000h")
	} else {
		screen.write("\x1b[?1006;1000l")
	}
}

// ShowCursorAt() moves the cursor to the given screen position and makes sure
// it is visible.
//
// If the position is outside of the screen, the cursor will be hidden.
func (screen *UnixScreen) ShowCursorAt(column int, row int) {
	if column < 0 {
		screen.hideCursor(true)
		return
	}
	if row < 0 {
		screen.hideCursor(true)
		return
	}

	width, height := screen.Size()
	if column >= width {
		screen.hideCursor(true)
		return
	}
	if row >= height {
		screen.hideCursor(true)
		return
	}

	// https://en.wikipedia.org/wiki/ANSI_escape_code#CSI_(Control_Sequence_Introducer)_sequences
	screen.write(fmt.Sprintf("\x1b[%d;%dH", row, column))
	screen.hideCursor(false)
}

func (screen *UnixScreen) mainLoop() {
	// "1400" comes from me trying fling scroll operations on my MacBook
	// trackpad and looking at the high watermark (logged below).
	//
	// The highest I saw when I tried this was 700 something. 1400 is twice
	// that, so 1400 should be good.
	buffer := make([]byte, 1400)

	log.Info("Entering Twin main loop...")

	maxBytesRead := 0
	expectingTerminalBackgroundColor := true
	var incompleteResponse []byte // To store incomplete terminal background color responses
	for {
		count, err := screen.ttyInReader.Read(buffer)
		if err != nil {
			// Ref:
			// * https://github.com/walles/moor/issues/145
			// * https://github.com/walles/moor/issues/149
			// * https://github.com/walles/moor/issues/150
			log.Info("ttyin read error, twin giving up: ", err)

			screen.events <- EventExit{}
			return
		}

		if expectingTerminalBackgroundColor {
			incompleteResponse = append(incompleteResponse, buffer[:count]...)
			// This is the response to our background color request
			bg, valid := parseTerminalBgColorResponse(incompleteResponse)
			if valid {
				if bg != nil {
					screen.terminalBackgroundLock.Lock()
					screen.terminalBackground = bg
					log.Debug("Terminal background color detected as ", bg, " after ", time.Since(*screen.terminalBackgroundQuery))
					screen.terminalBackgroundLock.Unlock()

					expectingTerminalBackgroundColor = false
					incompleteResponse = nil
				}
				continue
			}

			// Not valid, give up
			expectingTerminalBackgroundColor = false
			incompleteResponse = nil
		}

		if count > maxBytesRead {
			maxBytesRead = count
			log.Trace("ttyin high watermark bumped to ", maxBytesRead, " bytes")
		}

		encodedKeyCodeSequences := string(buffer[0:count])
		if !utf8.ValidString(encodedKeyCodeSequences) {
			log.Warn("Got invalid UTF-8 sequence on ttyin: ", encodedKeyCodeSequences)
			continue
		}

		for len(encodedKeyCodeSequences) > 0 {
			var event *Event
			event, encodedKeyCodeSequences = consumeEncodedEvent(encodedKeyCodeSequences)

			if event == nil {
				// No event, go wait for more
				break
			}

			// Post the event
			select {
			case screen.events <- *event:
				// Yay
			default:
				// If this happens, consider increasing the channel size in
				// NewScreen()
				log.Debugf("Events buffer (size %d) full, events are being dropped", cap(screen.events))
			}
		}
	}
}

// Turn ESC into <0x1b> and other low ASCII characters into <0xXX> for logging
// purposes.
func humanizeLowASCII(withLowAsciis string) string {
	humanized := ""
	for _, char := range withLowAsciis {
		if char < ' ' {
			humanized += fmt.Sprintf("<0x%2x>", char)
			continue
		}
		humanized += string(char)
	}
	return humanized
}

// Consume initial key code from the sequence of encoded keycodes.
//
// Returns a (possibly nil) event that should be posted, and the remainder of
// the encoded events sequence.
func consumeEncodedEvent(encodedEventSequences string) (*Event, string) {
	for singleKeyCodeSequence, keyCode := range escapeSequenceToKeyCode {
		if !strings.HasPrefix(encodedEventSequences, singleKeyCodeSequence) {
			continue
		}

		// Encoded key code sequence found, report it!
		var event Event = EventKeyCode{keyCode}
		return &event, strings.TrimPrefix(encodedEventSequences, singleKeyCodeSequence)
	}

	mouseMatch := mouseEventRegex.FindStringSubmatch(encodedEventSequences)
	if mouseMatch != nil {
		if mouseMatch[1] == "64" {
			var event Event = EventMouse{buttons: MouseWheelUp}
			return &event, strings.TrimPrefix(encodedEventSequences, mouseMatch[0])
		}
		if mouseMatch[1] == "65" {
			var event Event = EventMouse{buttons: MouseWheelDown}
			return &event, strings.TrimPrefix(encodedEventSequences, mouseMatch[0])
		}

		log.Debug(
			"Unhandled multi character mouse escape sequence(s): {",
			humanizeLowASCII(encodedEventSequences),
			"}")
		return nil, ""
	}

	// No escape sequence prefix matched
	runes := []rune(encodedEventSequences)
	if len(runes) == 0 {
		return nil, ""
	}

	if runes[0] == '\x1b' {
		if len(runes) != 1 {
			// This means one or more sequences should be added to
			// escapeSequenceToKeyCode in keys.go.
			log.Debug(
				"Unhandled multi character terminal escape sequence(s): {",
				humanizeLowASCII(encodedEventSequences),
				"}")

			// Mark everything as consumed since we don't know how to proceed otherwise.
			return nil, ""
		}

		var event Event = EventKeyCode{KeyEscape}
		return &event, string(runes[1:])
	}

	if runes[0] == '\r' {
		var event Event = EventKeyCode{KeyEnter}
		return &event, string(runes[1:])
	}

	// Report the single rune
	var event Event = EventRune{rune: runes[0]}
	return &event, string(runes[1:])
}

// Returns screen width and height.
//
// NOTE: Never cache this response! On window resizes you'll get an EventResize
// on the Screen.Events channel, and this method will start returning the new
// size instead.
func (screen *UnixScreen) Size() (width int, height int) {
	select {
	case <-screen.sigwinch:
		// Resize logic needed, see below
	default:
		// No resize, go with the existing values
		if screen.widthAccessFromSizeOnly == 0 || screen.heightAccessFromSizeOnly == 0 {
			panic(fmt.Sprintf("No screen size available, this is a bug: %d x %d",
				screen.widthAccessFromSizeOnly,
				screen.heightAccessFromSizeOnly))
		}
		return screen.widthAccessFromSizeOnly, screen.heightAccessFromSizeOnly
	}

	// Window was resized
	width, height, err := term.GetSize(int(screen.ttyOut.Fd()))
	if err != nil {
		panic(err)
	}

	if width == 0 || height == 0 {
		panic(fmt.Sprintf("Got zero screen size: %d x %d", width, height))
	}

	if screen.widthAccessFromSizeOnly == width && screen.heightAccessFromSizeOnly == height {
		// Not sure when this would happen, but if it does this wasn't really a
		// resize, and we don't need to treat it as such.
		return screen.widthAccessFromSizeOnly, screen.heightAccessFromSizeOnly
	}

	newCells := make([][]StyledRune, height)
	for rowNumber := 0; rowNumber < height; rowNumber++ {
		newCells[rowNumber] = make([]StyledRune, width)
	}

	// FIXME: Copy any existing contents over to the new, resized screen array
	// FIXME: Fill any non-initialized cells with whitespace

	screen.widthAccessFromSizeOnly = width
	screen.heightAccessFromSizeOnly = height
	screen.cells = newCells

	return screen.widthAccessFromSizeOnly, screen.heightAccessFromSizeOnly
}

// The first time you call this, there may be a delay of up to 50ms while we
// wait for the terminal to respond to our background color query. After that,
// it will be instant.
//
// Returns the terminal background color if known, nil otherwise.
func (screen *UnixScreen) TerminalBackground() *Color {
	const maxWait = 50 * time.Millisecond

	// Is it already known?
	screen.terminalBackgroundLock.Lock()
	if screen.terminalBackground != nil || time.Since(*screen.terminalBackgroundQuery) > maxWait {
		// Either we know the color or we gave up waiting for it. Return it!
		background := screen.terminalBackground
		screen.terminalBackgroundLock.Unlock()
		return background
	}
	screen.terminalBackgroundLock.Unlock()

	// Wait at most 50ms in total for the background to be detected
	screen.terminalBackgroundLock.Lock()
	start := screen.terminalBackgroundQuery
	screen.terminalBackgroundLock.Unlock()

	for time.Since(*start) < maxWait {
		screen.terminalBackgroundLock.Lock()
		if screen.terminalBackground != nil {
			// There it is!
			background := screen.terminalBackground
			screen.terminalBackgroundLock.Unlock()
			return background
		}

		// Unlock so the other goroutine can set it
		screen.terminalBackgroundLock.Unlock()

		// It's not more urgent than this
		time.Sleep(5 * time.Millisecond)
	}

	// The wait is over, return whatever we have
	screen.terminalBackgroundLock.Lock()
	defer screen.terminalBackgroundLock.Unlock()
	return screen.terminalBackground
}

func parseTerminalBgColorResponse(responseBytes []byte) (*Color, bool) {
	prefix := "\x1b]11;rgb:"
	suffix1 := "\x07"
	suffix2 := "\x1b\\"
	sampleResponse1 := prefix + "0000/0000/0000" + suffix1
	sampleResponse2 := prefix + "0000/0000/0000" + suffix2

	response := string(responseBytes)
	if !strings.HasPrefix(response, prefix) {
		log.Info("Got unexpected prefix in bg color response from terminal: <", humanizeLowASCII(string(responseBytes)), ">")
		return nil, false // Invalid
	}
	response = strings.TrimPrefix(response, prefix)

	isComplete := strings.HasSuffix(response, suffix1) || strings.HasSuffix(response, suffix2)
	if !isComplete && (len(responseBytes) < len(sampleResponse1) || len(responseBytes) < len(sampleResponse2)) {
		log.Trace("Terminal bg color response received so far: <", humanizeLowASCII(response), ">")
		return nil, true // Incomplete but valid
	}

	if !isComplete {
		log.Info("Got unexpected suffix in bg color response from terminal: <", humanizeLowASCII(string(responseBytes)), ">")
		return nil, false // Invalid
	}
	response = strings.TrimSuffix(response, suffix1)
	response = strings.TrimSuffix(response, suffix2)

	if len(response) != 14 {
		log.Info("Got unexpected length bg color response from terminal: <", humanizeLowASCII(string(responseBytes)), ">")
		return nil, false // Invalid
	}

	// response is now "RRRR/GGGG/BBBB"
	red, err := strconv.ParseUint(response[0:4], 16, 16)
	if err != nil {
		log.Info("Failed parsing red in bg color response from terminal: <", humanizeLowASCII(string(responseBytes)), ">: ", err)
		return nil, false // Invalid
	}

	green, err := strconv.ParseUint(response[5:9], 16, 16)
	if err != nil {
		log.Info("Failed parsing green in bg color response from terminal: <", humanizeLowASCII(string(responseBytes)), ">: ", err)
		return nil, false // Invalid
	}

	blue, err := strconv.ParseUint(response[10:14], 16, 16)
	if err != nil {
		log.Info("Failed parsing blue in bg color response from terminal: <", humanizeLowASCII(string(responseBytes)), ">: ", err)
		return nil, false // Invalid
	}

	color := NewColor24Bit(uint8(red/256), uint8(green/256), uint8(blue/256))

	return &color, true // Valid
}

func (screen *UnixScreen) SetCell(column int, row int, styledRune StyledRune) int {
	if column < 0 {
		return styledRune.Width()
	}
	if row < 0 {
		return styledRune.Width()
	}

	width, height := screen.Size()
	if column >= width {
		return styledRune.Width()
	}
	if row >= height {
		return styledRune.Width()
	}

	if column+styledRune.Width() > width {
		// This cell is too wide for the screen, write a space instead
		screen.cells[row][column] = NewStyledRune(' ', styledRune.Style)
		return styledRune.Width()
	}

	screen.cells[row][column] = styledRune

	return styledRune.Width()
}

func (screen *UnixScreen) Clear() {
	empty := NewStyledRune(' ', StyleDefault)

	width, height := screen.Size()
	for row := 0; row < height; row++ {
		for column := 0; column < width; column++ {
			screen.cells[row][column] = empty
		}
	}
}

// A cell is considered hidden if it's preceded by a wide character that spans
// multiple columns.
func withoutHiddenRunes(runes []StyledRune) []StyledRune {
	result := make([]StyledRune, 0, len(runes))

	for i := 0; i < len(runes); i++ {
		if i > 0 && runes[i-1].Width() == 2 {
			// This is a hidden rune
			continue
		}

		result = append(result, runes[i])
	}

	return result
}

// Returns the rendered line, plus how many information carrying cells went into
// it. The width is used to decide whether or not to clear to EOL at the end of
// the line.
func renderLine(row []StyledRune, width int, terminalColorCount ColorCount) (string, int) {
	row = withoutHiddenRunes(row)

	// Strip trailing whitespace
	trailerBg := ColorDefault
	trailerBgSet := false
	lastSignificantCellIndex := len(row) - 1
	for ; lastSignificantCellIndex >= 0; lastSignificantCellIndex-- {
		lastCell := row[lastSignificantCellIndex]
		if lastCell.Rune != ' ' {
			break
		}

		whiteSpaceBg := lastCell.Style.bg
		if lastCell.Style.attrs.has(AttrReverse) {
			// Style is inverted, take the foreground color instead
			if lastCell.Style.fg == ColorDefault {
				// We don't know what the default color is in this case, so we
				// can't use it.
				break
			}

			whiteSpaceBg = lastCell.Style.fg
		}

		if !trailerBgSet {
			trailerBg = whiteSpaceBg
			trailerBgSet = true
		}

		if whiteSpaceBg != trailerBg {
			break
		}
	}
	row = row[0 : lastSignificantCellIndex+1]

	var builder strings.Builder

	// Set initial line style to normal
	builder.WriteString("\x1b[m")
	lastStyle := StyleDefault

	for _, cell := range row {
		style := cell.Style
		runeToWrite := cell.Rune
		if !Printable(runeToWrite) {
			// Highlight unprintable runes
			style = Style{
				fg:    NewColor16(7), // White
				bg:    NewColor16(1), // Red
				attrs: AttrBold,
			}
			runeToWrite = '?'
		}

		if style != lastStyle {
			builder.WriteString(style.RenderUpdateFrom(lastStyle, terminalColorCount))
			lastStyle = style
		}

		builder.WriteRune(runeToWrite)
	}

	lastStyleMinusHyperlink := lastStyle.WithHyperlink(nil)
	if lastStyleMinusHyperlink != lastStyle {
		// Remove the hyperlink attribute
		builder.WriteString(lastStyleMinusHyperlink.RenderUpdateFrom(lastStyle, terminalColorCount))
		lastStyle = lastStyleMinusHyperlink
	}

	if len(row) < width {
		// Clear to end of line
		// https://en.wikipedia.org/wiki/ANSI_escape_code#CSI_(Control_Sequence_Introducer)_sequences
		//
		// Note that we can't do this if we're one the last screen column:
		// https://github.com/microsoft/terminal/issues/18115#issuecomment-2448054645
		builder.WriteString(StyleDefault.WithBackground(trailerBg).RenderUpdateFrom(lastStyle, terminalColorCount))
		builder.WriteString("\x1b[K")
	}

	return builder.String(), len(row)
}

func (screen *UnixScreen) Show() {
	width, height := screen.Size()
	screen.showNLines(width, height, true)
}

func (screen *UnixScreen) ShowNLines(height int) {
	width, _ := screen.Size()
	screen.showNLines(width, height, false)
}

func (screen *UnixScreen) showNLines(width int, height int, clearFirst bool) {
	var builder strings.Builder

	if clearFirst {
		// Start in the top left corner:
		// https://en.wikipedia.org/wiki/ANSI_escape_code#CSI_(Control_Sequence_Introducer)_sequences
		builder.WriteString("\x1b[1;1H")
	}

	for row := range height {
		rendered, lineLength := renderLine(screen.cells[row], width, screen.terminalColorCount)
		builder.WriteString(rendered)

		wasLastLine := row == (height - 1)

		// NOTE: This <= should *really* be <= and nothing else. Otherwise, if
		// one line precisely as long as the terminal window goes before one
		// empty line, the empty line will never be rendered.
		//
		// Can be demonstrated using "moor m/pager.go", scroll right once to
		// make the line numbers go away, then make the window narrower until
		// some line before an empty line is just as wide as the window.
		//
		// With the wrong comparison here, then the empty line just disappears.
		if lineLength <= len(screen.cells[row]) && !wasLastLine {
			builder.WriteString("\r\n")
		}
	}

	// Write out what we have
	screen.write(builder.String())
}
