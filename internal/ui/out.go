package ui

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

// Output for every command, so aft reads the same everywhere. Results and data
// go to stdout; warnings, errors and the live spinner go to stderr. Colors are
// the terminal's own ANSI colors, so they follow its theme, and each stream
// drops them when it is not a terminal or NO_COLOR is set. Only the 16 colors
// the terminal's theme defines are used, so they are the user's own, and
// secondary text is faint rather than gray: faint is the user's text color,
// dimmed, which stays readable on any theme where "bright black" may not. A
// mark always comes with its color, so nothing depends on color alone.
var (
	stdout = lipgloss.NewRenderer(os.Stdout)
	stderr = lipgloss.NewRenderer(os.Stderr)
)

// Marks for results; each pairs a symbol with a color.
const (
	markOK   = "✓"
	markWarn = "!"
	markBad  = "✗"
	markInfo = "·"
)

func style(r *lipgloss.Renderer, color string) lipgloss.Style {
	return r.NewStyle().Foreground(lipgloss.Color(color))
}

func faint(r *lipgloss.Renderer) lipgloss.Style { return r.NewStyle().Faint(true) }

// Bold, Dim and Accent style text for stdout.
func Bold(s string) string   { return stdout.NewStyle().Bold(true).Render(s) }
func Dim(s string) string    { return faint(stdout).Render(s) }
func Accent(s string) string { return style(stdout, "6").Render(s) }

// Mark returns the colored mark for a level: ok, warn, fix or info.
func Mark(level string) string { return mark(stdout, level) }

func mark(r *lipgloss.Renderer, level string) string {
	switch level {
	case "ok":
		return style(r, "2").Render(markOK)
	case "warn":
		return style(r, "3").Render(markWarn)
	case "fix":
		return style(r, "1").Render(markBad)
	}
	return faint(r).Render(markInfo)
}

// OK reports a finished action on stdout.
func OK(format string, a ...any) {
	fmt.Println(Mark("ok") + " " + fmt.Sprintf(format, a...))
}

// Note prints a short line to stderr, for telling the user what changed.
func Note(format string, a ...any) {
	fmt.Fprintln(os.Stderr, style(stderr, "6").Render("•")+" "+fmt.Sprintf(format, a...))
}

// Warn prints a warning to stderr.
func Warn(format string, a ...any) {
	fmt.Fprintln(os.Stderr, mark(stderr, "warn")+" "+fmt.Sprintf(format, a...))
}

// Error prints an error to stderr.
func Error(format string, a ...any) {
	fmt.Fprintln(os.Stderr, mark(stderr, "fix")+" "+fmt.Sprintf(format, a...))
}

// Hint prints the next thing to do, indented under the line above, to stderr.
// It is in the user's own text color: a hint is what they should read next.
func Hint(format string, a ...any) {
	fmt.Fprintln(os.Stderr, "  "+style(stderr, "6").Render("→")+" "+fmt.Sprintf(format, a...))
}

// Suggest prints, under an error, the command the user probably meant, in
// bold accent so it is not skimmed past.
func Suggest(cmd string) {
	fmt.Fprintln(os.Stderr, "  "+style(stderr, "6").Render("→")+" did you mean "+style(stderr, "6").Bold(true).Render(cmd)+"?")
}

// Command styles a command the user can copy.
func Command(s string) string { return style(stdout, "6").Bold(true).Render(s) }

// Path shortens a local path under the home directory to ~.
func Path(p string) string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return p
	}
	if p == h {
		return "~"
	}
	if strings.HasPrefix(p, h+string(filepath.Separator)) {
		return "~" + p[len(h):]
	}
	return p
}

// Ago renders how long ago t was, coarsely: "just now", "5m ago", "3h ago".
func Ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// Table prints rows in aligned columns under a dim header. Cells may carry
// styles; widths ignore them.
func Table(header []string, rows [][]string) {
	widths := make([]int, len(header))
	for _, r := range append([][]string{header}, rows...) {
		for i, c := range r {
			if w := lipgloss.Width(c); i < len(widths) && w > widths[i] {
				widths[i] = w
			}
		}
	}
	line := func(r []string, dim bool) {
		var b strings.Builder
		for i, c := range r {
			if dim {
				c = Dim(c)
			}
			b.WriteString(c)
			if i < len(r)-1 && i < len(widths) {
				b.WriteString(strings.Repeat(" ", max(widths[i]-lipgloss.Width(c), 0)+2))
			}
		}
		fmt.Println(b.String())
	}
	line(header, true)
	for _, r := range rows {
		line(r, false)
	}
}

// Live is a spinner on stderr with an optional step count and bar, for work
// that takes a moment. Lines printed through it land above the spinner, so a
// clean log remains when it stops. When stderr is not a terminal it draws
// nothing and only passes lines through.
type Live struct {
	mu          sync.Mutex
	tty         bool
	label       string
	done, total int
	frame       int
	paused      bool
	stop, ended chan struct{}
	sig         chan os.Signal
}

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Start begins a spinner showing label. With total > 0 it also shows how many
// of total steps are done; Advance counts them.
func Start(label string, total int) *Live {
	l := &Live{label: label, total: total, tty: term.IsTerminal(int(os.Stderr.Fd())), stop: make(chan struct{}), ended: make(chan struct{})}
	if !l.tty {
		close(l.ended)
		return l
	}
	// Restore the cursor if interrupted while it is hidden.
	l.sig = make(chan os.Signal, 1)
	signal.Notify(l.sig, os.Interrupt)
	fmt.Fprint(os.Stderr, "\x1b[?25l")
	go l.run()
	return l
}

func (l *Live) run() {
	defer close(l.ended)
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	for {
		l.mu.Lock()
		l.draw()
		l.mu.Unlock()
		select {
		case <-l.stop:
			return
		case <-l.sig:
			l.mu.Lock()
			fmt.Fprint(os.Stderr, "\r\x1b[K\x1b[?25h")
			os.Exit(130)
		case <-t.C:
			l.frame++
		}
	}
}

// draw renders the spinner line; the caller holds mu.
func (l *Live) draw() {
	if l.paused {
		return
	}
	line := style(stderr, "6").Render(spinFrames[l.frame%len(spinFrames)]) + " " + l.label
	if l.total > 0 {
		const width = 16
		n := width * l.done / l.total
		bar := style(stderr, "6").Render(strings.Repeat("━", n)) + faint(stderr).Render(strings.Repeat("─", width-n))
		line += "  " + bar + " " + faint(stderr).Render(fmt.Sprintf("%d/%d", l.done, l.total))
	}
	// A line wider than the terminal would wrap and break the redraw.
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 && lipgloss.Width(line) >= w {
		line = ansi.Truncate(line, w-1, "")
	}
	fmt.Fprint(os.Stderr, "\r\x1b[K"+line)
}

// Step changes what the spinner says is happening.
func (l *Live) Step(label string) {
	l.mu.Lock()
	l.label = label
	l.mu.Unlock()
}

// Advance counts one finished step.
func (l *Live) Advance() {
	l.mu.Lock()
	if l.done < l.total {
		l.done++
	}
	l.mu.Unlock()
}

// Println prints a finished line to stdout above the spinner.
func (l *Live) Println(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tty && !l.paused {
		fmt.Fprint(os.Stderr, "\r\x1b[K")
	}
	fmt.Println(s)
}

// Pause clears the spinner so something else can use the terminal, such as an
// SSH passphrase prompt; Resume brings it back.
func (l *Live) Pause() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tty && !l.paused {
		fmt.Fprint(os.Stderr, "\r\x1b[K\x1b[?25h")
	}
	l.paused = true
}

func (l *Live) Resume() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tty && l.paused {
		fmt.Fprint(os.Stderr, "\x1b[?25l")
	}
	l.paused = false
}

// Stop clears the spinner and shows the cursor again.
func (l *Live) Stop() {
	if !l.tty {
		return
	}
	select {
	case <-l.stop:
		return
	default:
		close(l.stop)
	}
	<-l.ended
	signal.Stop(l.sig)
	fmt.Fprint(os.Stderr, "\r\x1b[K\x1b[?25h")
}

// Spin shows label with a spinner while fn runs.
func Spin(label string, fn func() error) error {
	l := Start(label, 0)
	defer l.Stop()
	return fn()
}
