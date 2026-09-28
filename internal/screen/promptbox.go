// Package screen reads an agent's pane (`herdr agent read --source visible
// --format ansi`) to decide whether text may be typed into it: the input box
// must not hold someone's unsent draft, and the pane must not show a trust
// dialog. It works for every harness without hooks: each supported kind has
// its own anchor for the box, and text drawn dim, a known placeholder, or a
// harness-drawn cursor over a placeholder does not count as typed.
//
// Ported from herdr-projects (src/prompt_box.rs, src/trust_screen.rs),
// Copyright (c) 2026 Elias Stravik, MIT License; see testdata/NOTICE.md.
package screen

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Draft is what an agent's input box holds.
type Draft int

const (
	// Empty: the box is on screen and holds nothing typed.
	Empty Draft = iota
	// Typed: someone typed text that was not submitted.
	Typed
	// Unknown: no input box was found (a menu or dialog, a scrolled view,
	// a layout this package does not know, an unsupported kind).
	Unknown
)

func (d Draft) String() string {
	switch d {
	case Empty:
		return "empty"
	case Typed:
		return "typed"
	default:
		return "unknown"
	}
}

// Check reads the input box of an agent of kind from its screen.
func Check(kind, screen string) Draft {
	text, ok := boxText(kind, screen)
	switch {
	case !ok:
		return Unknown
	case text == "":
		return Empty
	default:
		return Typed
	}
}

// placeholders are drawn in a normal (not dim) colour, per kind. Only
// strings nobody types by accident.
func placeholders(kind string) []string {
	switch kind {
	case "gemini":
		return []string{"Type your message or @path/to/file"}
	case "opencode":
		return []string{"Ask anything…"}
	}
	return nil
}

type cell struct {
	ch      rune
	dim     bool
	reverse bool
	bg      string
}

type line []cell

func (l line) text() string {
	var b strings.Builder
	for _, c := range l {
		b.WriteRune(c.ch)
	}
	return b.String()
}

// parse splits screen text into styled cells. Only SGR dim, reverse and the
// background colour are kept; other escape sequences are skipped.
func parse(screen string) []line {
	var lines []line
	var cur line
	var dim, reverse bool
	var bg string
	rs := []rune(screen)
	for i := 0; i < len(rs); i++ {
		ch := rs[i]
		switch {
		case ch == 0x1b:
			if i+1 >= len(rs) || rs[i+1] != '[' {
				i++
				continue
			}
			i += 2
			start, final := i, rune(' ')
			for ; i < len(rs); i++ {
				if rs[i] >= 0x40 && rs[i] <= 0x7e {
					final = rs[i]
					break
				}
			}
			if final == 'm' {
				sgr(string(rs[start:i]), &dim, &reverse, &bg)
			}
		case ch == '\n':
			lines = append(lines, cur)
			cur = nil
		case unicode.IsControl(ch):
		default:
			cur = append(cur, cell{ch: ch, dim: dim, reverse: reverse, bg: bg})
		}
	}
	if len(cur) > 0 {
		lines = append(lines, cur)
	}
	return lines
}

func sgr(params string, dim, reverse *bool, bg *string) {
	codes := []string{"0"}
	if params != "" {
		codes = strings.Split(params, ";")
	}
	for i := 0; i < len(codes); i++ {
		switch code := codes[i]; {
		case code == "0" || code == "":
			*dim, *reverse, *bg = false, false, ""
		case code == "2":
			*dim = true
		case code == "22":
			*dim = false
		case code == "7":
			*reverse = true
		case code == "27":
			*reverse = false
		case code == "49":
			*bg = ""
		case code == "38" || code == "48":
			take := 0
			if i+1 < len(codes) {
				switch codes[i+1] {
				case "5":
					take = 2
				case "2":
					take = 4
				}
			}
			if code == "48" {
				end := min(i+1+take, len(codes))
				*bg = strings.Join(codes[i+1:end], ";")
			}
			i += take
		case len(code) == 2 && code[0] == '4':
			*bg = code
		case len(code) == 3 && strings.HasPrefix(code, "10"):
			*bg = code
		}
	}
}

// after returns the cells after the first glyph on l, the anchor dropped.
func after(l line, glyph rune) (line, bool) {
	for i, c := range l {
		if c.ch == glyph {
			return append(line(nil), l[i+1:]...), true
		}
	}
	return nil, false
}

func trimmedStarts(l line, prefix string) bool {
	return strings.HasPrefix(strings.TrimLeftFunc(l.text(), unicode.IsSpace), prefix)
}

// isRule reports a horizontal rule: a line of box-drawing `─` only.
func isRule(l line) bool {
	t := strings.TrimSpace(l.text())
	return utf8.RuneCountInString(t) >= 10 && strings.Trim(t, "─") == ""
}

// isBorder reports a box border: a rule, or a rule that opens with at least
// ten `─` and carries a label (Claude Code puts the session title there:
// `──── <title> ─`).
func isBorder(l line) bool {
	if isRule(l) {
		return true
	}
	t := strings.TrimSpace(l.text())
	return strings.HasPrefix(t, strings.Repeat("─", 10))
}

// typed is the text in a box's cells: non-blank, not dim, and not a
// harness-drawn cursor sitting on a dim placeholder.
func typed(cells line) string {
	var b strings.Builder
	for i, c := range cells {
		if c.dim || unicode.IsSpace(c.ch) || (c.reverse && i+1 < len(cells) && cells[i+1].dim) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(c.ch)
	}
	return b.String()
}

// lastIndex is the last line index matching pred, or -1.
func lastIndex(lines []line, pred func(int, line) bool) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if pred(i, lines[i]) {
			return i
		}
	}
	return -1
}

// inputBox returns the box's cells, one entry per line, or false when it
// is not on the screen.
func inputBox(kind string, lines []line) ([]line, bool) {
	oneRow := func(glyph string) ([]line, bool) {
		at := lastIndex(lines, func(_ int, l line) bool { return trimmedStarts(l, glyph) })
		if at < 0 {
			return nil, false
		}
		row, ok := after(lines[at], []rune(glyph)[0])
		return []line{row}, ok
	}
	switch kind {
	// `❯` right under a border, continued until the next border.
	case "claude":
		at := lastIndex(lines, func(i int, l line) bool { return trimmedStarts(l, "❯") && i > 0 && isBorder(lines[i-1]) })
		if at < 0 {
			return nil, false
		}
		end := -1
		for i := at + 1; i < len(lines); i++ {
			if isBorder(lines[i]) {
				end = i
				break
			}
		}
		if end < 0 {
			return nil, false
		}
		first, ok := after(lines[at], '❯')
		if !ok {
			return nil, false
		}
		return append([]line{first}, lines[at+1:end]...), true
	case "codex":
		return oneRow("›")
	case "cursor":
		return oneRow("→")
	// `│ > ` (or `!` shell mode, `*` yolo mode) in a bordered box.
	case "gemini":
		at := lastIndex(lines, func(_ int, l line) bool {
			t := strings.TrimLeftFunc(l.text(), unicode.IsSpace)
			rest, ok := strings.CutPrefix(t, "│")
			if !ok {
				return false
			}
			r, _ := utf8.DecodeRuneInString(strings.TrimLeftFunc(rest, unicode.IsSpace))
			return r == '>' || r == '!' || r == '*'
		})
		if at < 0 {
			return nil, false
		}
		end := -1
		for i := at + 1; i < len(lines); i++ {
			if trimmedStarts(lines[i], "╰") {
				end = i
				break
			}
		}
		if end < 0 {
			return nil, false
		}
		var rows []line
		for n, l := range lines[at:end] {
			row, ok := after(l, '│')
			if !ok {
				return nil, false
			}
			for i := len(row) - 1; i >= 0; i-- {
				if row[i].ch == '│' {
					row = row[:i]
					break
				}
			}
			if n == 0 {
				prompt := -1
				for i, c := range row {
					if c.ch == '>' || c.ch == '!' || c.ch == '*' {
						prompt = i
						break
					}
				}
				if prompt < 0 {
					return nil, false
				}
				row = row[prompt+1:]
			}
			rows = append(rows, row)
		}
		return rows, true
	// The `┃` lines above the box's `╹▀▀▀` bottom, less the last one (the
	// mode and model line); only cells on the box's own background count,
	// so the session sidebar to its right is left out.
	case "opencode":
		bottom := lastIndex(lines, func(_ int, l line) bool { return trimmedStarts(l, "╹") })
		if bottom < 0 {
			return nil, false
		}
		top := -1
		for i := bottom - 1; i >= 0 && trimmedStarts(lines[i], "┃"); i-- {
			top = i
		}
		if top < 0 || bottom-top < 2 {
			return nil, false
		}
		var rows []line
		for _, l := range lines[top : bottom-1] {
			row, _ := after(l, '┃')
			var kept line
			for _, c := range row {
				if c.bg != row[0].bg {
					break
				}
				kept = append(kept, c)
			}
			rows = append(rows, kept)
		}
		return rows, true
	// The editor between the last two rules.
	case "pi":
		bottom := lastIndex(lines, func(_ int, l line) bool { return isRule(l) })
		if bottom < 0 {
			return nil, false
		}
		top := lastIndex(lines[:bottom], func(_ int, l line) bool { return isRule(l) })
		if top < 0 {
			return nil, false
		}
		return lines[top+1 : bottom], true
	}
	return nil, false
}

// boxText is the text typed in the input box, its lines joined by spaces
// (empty for an empty box), or false when the box is not on the screen.
func boxText(kind, screen string) (string, bool) {
	rows, ok := inputBox(kind, parse(screen))
	if !ok {
		return "", false
	}
	var parts []string
	for _, row := range rows {
		if t := strings.TrimSpace(typed(row)); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 1 {
		for _, p := range placeholders(kind) {
			if strings.HasPrefix(parts[0], p) {
				return "", true
			}
		}
	}
	return strings.Join(parts, " "), true
}
