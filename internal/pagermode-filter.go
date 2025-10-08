package internal

import (
	log "github.com/sirupsen/logrus"
	"github.com/walles/moor/v2/twin"
)

type PagerModeFilter struct {
	pager    *Pager
	inputBox *InputBox
}

func NewPagerModeFilter(p *Pager) *PagerModeFilter {
	m := &PagerModeFilter{
		pager: p,
	}
	m.inputBox = &InputBox{
		accept: INPUTBOX_ACCEPT_ALL,
		onTextChanged: func(text string) {
			m.updateFilterPattern(text)
		},
	}
	return m
}

func (m PagerModeFilter) drawFooter(_ string, _ string) {
	m.inputBox.draw(m.pager.screen, "Filter: ")
}

func (m *PagerModeFilter) updateFilterPattern(text string) {
	m.pager.filterPattern = toPattern(text)
	m.pager.searchString = text
	m.pager.searchPattern = toPattern(text)
}

func (m *PagerModeFilter) onKey(key twin.KeyCode) {
	if m.inputBox.handleKey(key) {
		return
	}

	switch key {
	case twin.KeyEnter:
		m.pager.mode = PagerModeViewing{pager: m.pager}

	case twin.KeyEscape:
		m.pager.mode = PagerModeViewing{pager: m.pager}
		m.pager.filterPattern = nil
		m.pager.searchString = ""
		m.pager.searchPattern = nil

	case twin.KeyUp, twin.KeyDown, twin.KeyPgUp, twin.KeyPgDown:
		viewing := PagerModeViewing{pager: m.pager}
		viewing.onKey(key)

	default:
		log.Debugf("Unhandled filter key event %v", key)
	}
}

func (m *PagerModeFilter) onRune(char rune) {
	m.inputBox.handleRune(char)
}
