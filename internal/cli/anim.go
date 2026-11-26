package cli

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Animation timing. The spinner waits before drawing so fast requests never
// flicker; banners are short so they never get in the way.
const (
	spinDelay     = 150 * time.Millisecond
	spinInterval  = 80 * time.Millisecond
	bannerFrames  = 20
	bannerFrameDt = 30 * time.Millisecond
	typeDelay     = 10 * time.Millisecond
)

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// gradientStops picks the colors a theme's animations sweep through.
func gradientStops(t Theme) []string {
	if len(t.Users) >= 2 {
		return t.Users
	}
	if t.Accent != "" {
		return []string{t.Accent, t.Mention}
	}
	return nil
}

// Spin shows an animated spinner with label on stderr until the returned stop
// func is called. It only animates when stderr is a terminal and animations
// are on; otherwise it is a no-op. The spinner appears only after spinDelay,
// and stop always erases it and restores the cursor before returning, so the
// caller can print right after without the two interleaving.
func (u *UI) Spin(label string) (stop func()) {
	if u == nil || !u.Anim || !u.ErrTTY || u.Err == nil {
		return func() {}
	}
	st := u.ErrStyle
	stopCh, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-stopCh:
			return
		case <-time.After(spinDelay):
		}
		fmt.Fprint(u.Err, "\x1b[?25l") // hide cursor
		tick := time.NewTicker(spinInterval)
		defer tick.Stop()
		for i := 0; ; i++ {
			frame := st.paint(spinFrames[i%len(spinFrames)], st.Theme().Accent, "1")
			fmt.Fprintf(u.Err, "\r\x1b[K%s %s", frame, st.Gradient(label, float64(i%40)/40, gradientStops(st.Theme())...))
			select {
			case <-stopCh:
				fmt.Fprint(u.Err, "\r\x1b[K\x1b[?25h") // erase + show cursor
				return
			case <-tick.C:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stopCh)
			<-done
		})
	}
}

// Banner prints a one-line title. On an animating terminal the title's colors
// sweep across it for a moment before settling; with color only, it is drawn
// once in the gradient; in plain mode it is printed as is.
func (u *UI) Banner(out io.Writer, text string) {
	st := u.style()
	if !st.Enabled() {
		fmt.Fprintln(out, text)
		return
	}
	stops := gradientStops(st.Theme())
	if u.Anim && u.OutTTY {
		fmt.Fprint(out, "\x1b[?25l")
		for f := 0; f < bannerFrames; f++ {
			fmt.Fprintf(out, "\r\x1b[K%s", st.Gradient(text, float64(f)/bannerFrames, stops...))
			time.Sleep(bannerFrameDt)
		}
		fmt.Fprint(out, "\r\x1b[K\x1b[?25h")
	}
	fmt.Fprintln(out, st.Gradient(text, 0, stops...))
}

// Typewrite prints line one rune at a time on an animating terminal, else all
// at once. line may contain escape codes; they are emitted whole.
func (u *UI) Typewrite(out io.Writer, line string) {
	if u == nil || !u.Anim || !u.OutTTY {
		fmt.Fprintln(out, line)
		return
	}
	inEsc := false
	for _, r := range line {
		fmt.Fprint(out, string(r))
		switch {
		case r == 0x1b:
			inEsc = true
		case inEsc && (r >= 0x40 && r <= 0x7e && r != '['):
			inEsc = false
		case !inEsc:
			time.Sleep(typeDelay)
		}
	}
	fmt.Fprintln(out)
}
