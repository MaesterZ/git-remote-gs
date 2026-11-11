package protocol

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// meterInterval is how often a meter redraws on its own, like git's one-second SIGALRM tick.
const meterInterval = time.Second

const percent = 100

// Meter counts the bytes of one transfer and shows them on one stderr line, redrawn every tick. A nil Meter does nothing.
type Meter struct {
	mu      sync.Mutex
	log     *Logger
	label   string
	bytes   int64
	total   int64     // expected bytes; 0 when unknown, which shows the byte count alone
	started time.Time // first byte, the start of the throughput
	stop    chan struct{}
	done    chan struct{}
}

// Meter returns a meter labelled label, or nil when `option progress` is off.
func (l *Logger) Meter(label string) *Meter {
	return l.newMeter(label, meterInterval)
}

func (l *Logger) newMeter(label string, interval time.Duration) *Meter {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.progress {
		return nil
	}

	m := &Meter{log: l, label: label, stop: make(chan struct{}), done: make(chan struct{})}

	go m.tick(interval)

	return m
}

// Reader counts the bytes read through r.
func (m *Meter) Reader(r io.Reader) io.Reader {
	if m == nil {
		return r
	}

	return io.TeeReader(r, m)
}

// Writer counts the bytes written through w.
func (m *Meter) Writer(w io.Writer) io.Writer {
	if m == nil {
		return w
	}

	return io.MultiWriter(w, m)
}

// Write counts data; the ticker redraws the line.
func (m *Meter) Write(data []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started.IsZero() {
		m.started = time.Now()
	}

	m.bytes += int64(len(data))

	return len(data), nil
}

// AddTotal adds n to the expected bytes, which turns the line into a percentage.
func (m *Meter) AddTotal(size int64) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.total += size
}

// Finish stops the ticker and ends the line with the total, as "<label>: <size> | <rate>/s, done."; nothing moved, nothing printed.
func (m *Meter) Finish() {
	if m == nil {
		return
	}

	close(m.stop)
	<-m.done

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.bytes > 0 {
		m.log.draw(m.label, m.bytes, m.total, m.rate(), true)
	}
}

// tick redraws the line every interval until Finish.
func (m *Meter) tick(interval time.Duration) {
	defer close(m.done)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.redraw()
		case <-m.stop:
			return
		}
	}
}

// redraw shows the line once bytes have moved; the throughput falls while a transfer stalls, so the line keeps changing.
func (m *Meter) redraw() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.bytes > 0 {
		m.log.draw(m.label, m.bytes, m.total, m.rate(), false)
	}
}

// rate returns the average throughput in bytes per second since the first byte. Call with m.mu held.
func (m *Meter) rate() int64 {
	if m.started.IsZero() {
		return 0
	}

	seconds := time.Since(m.started).Seconds()
	if seconds <= 0 {
		return 0
	}

	return int64(float64(m.bytes) / seconds)
}

// draw rewrites the meter line in place; done ends it with a newline.
func (l *Logger) draw(label string, size, total, rate int64, done bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	line := eraseLine + meterText(label, size, total, rate)

	if done {
		_, _ = fmt.Fprintf(l.w, "%s, done.\n", line)
		l.live = false

		return
	}

	_, _ = io.WriteString(l.w, line)
	l.live = true
}

// meterText is "<label>: <pct>% (<size>/<total>) | <rate>/s", or "<label>: <size> | <rate>/s" when total is unknown.
func meterText(label string, size, total, rate int64) string {
	if total == 0 {
		return fmt.Sprintf("%s: %s | %s/s", label, humanBytes(size), humanBytes(rate))
	}

	pct := min(size*percent/total, percent)

	return fmt.Sprintf("%s: %d%% (%s/%s) | %s/s", label, pct, humanBytes(size), humanBytes(total), humanBytes(rate))
}

// Stage prints a finished stage as "<label>: <detail>, done.", when progress is on.
func (l *Logger) Stage(label, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.progress {
		return
	}

	_, _ = fmt.Fprintf(l.w, "%s%s: %s, done.\n", eraseLine, label, detail)
	l.live = false
}

// humanBytes formats size with a binary unit, such as "1.5 MiB".
func humanBytes(size int64) string {
	const unit = 1024

	if size < unit {
		return fmt.Sprintf("%d bytes", size)
	}

	div, exp := int64(unit), 0
	for m := size / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}
