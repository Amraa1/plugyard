package cli

import (
	"io"
	"os"
	"strings"

	"github.com/Amraa1/plugyard/internal/charger"
	"github.com/Amraa1/plugyard/internal/ocpp/v16"
)

// palette colors text for a terminal. When it is off, text passes through
// unchanged, so color is never the only thing that carries meaning.
type palette struct{ on bool }

// newPalette turns color on only when w is a terminal that understands it
// and the user has not asked for plain output with NO_COLOR.
func newPalette(w io.Writer) palette {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return palette{}
	}
	f, ok := w.(*os.File)
	if !ok {
		return palette{}
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return palette{}
	}
	return palette{on: enableTerminalColors(f)}
}

func (p palette) wrap(code, s string) string {
	if !p.on {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p palette) green(s string) string  { return p.wrap("32", s) }
func (p palette) yellow(s string) string { return p.wrap("33", s) }
func (p palette) red(s string) string    { return p.wrap("31", s) }
func (p palette) bold(s string) string   { return p.wrap("1", s) }

// idColors are the colors charge point IDs are given in logs. Red is left
// out, since it means failure everywhere else.
var idColors = []string{"36", "35", "34", "33", "32", "96", "95", "94", "93", "92"}

// id colors a charge point ID. The same ID always gets the same color.
func (p palette) id(id string) string {
	if !p.on {
		return id
	}
	sum := uint32(2166136261)
	for i := 0; i < len(id); i++ {
		sum = (sum ^ uint32(id[i])) * 16777619
	}
	return p.wrap(idColors[sum%uint32(len(idColors))], id)
}

// connection colors a connection state.
func (p palette) connection(state string) string {
	switch state {
	case charger.Online:
		return p.green(state)
	case charger.Connecting:
		return p.yellow(state)
	case charger.Offline:
		return p.red(state)
	}
	return state
}

// status colors a connector status.
func (p palette) status(status string) string {
	switch status {
	case v16.StatusCharging:
		return p.green(status)
	case v16.StatusPreparing, v16.StatusFinishing, v16.StatusSuspendedEV, v16.StatusSuspendedEVSE:
		return p.yellow(status)
	case v16.StatusFaulted:
		return p.red(status)
	}
	return status
}

// cell is one table cell: its plain text, which sets the column width, and
// the possibly colored text to print.
type cell struct {
	plain    string
	rendered string
}

func plain(s string) cell { return cell{plain: s, rendered: s} }

// writeTable prints rows in aligned columns. Column widths come from the
// plain text, so color codes do not disturb the alignment.
func writeTable(w io.Writer, rows [][]cell) {
	var widths []int
	for _, row := range rows {
		for i, c := range row {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], len([]rune(c.plain)))
		}
	}

	var b strings.Builder
	for _, row := range rows {
		for i, c := range row {
			b.WriteString(c.rendered)
			if i < len(row)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-len([]rune(c.plain))+2))
			}
		}
		b.WriteByte('\n')
	}
	_, _ = io.WriteString(w, b.String())
}
