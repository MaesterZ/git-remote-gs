package protocol //nolint:testpackage // white-box: exercises the unexported meter line format

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestMeterOffIsNoop(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	log := NewLogger(&out, false)

	meter := log.Meter("Uploading")
	if meter != nil {
		t.Fatalf("meter with progress off: got %v, want nil", meter)
	}

	src := strings.NewReader("data")
	if meter.Reader(src) != io.Reader(src) {
		t.Error("nil meter must return the reader unchanged")
	}

	meter.Finish()

	if out.Len() != 0 {
		t.Errorf("output with progress off: %q", out.String())
	}
}

func TestMeterFinishesWithTotal(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	log := NewLogger(&out, false)
	log.SetProgress(true)

	meter := log.Meter("Uploading")

	_, err := io.Copy(io.Discard, meter.Reader(strings.NewReader(strings.Repeat("x", 2048))))
	if err != nil {
		t.Fatal(err)
	}

	meter.Finish()

	done := regexp.MustCompile(`\x1b\[KUploading: 2\.0 KiB \| [0-9.]+ [A-Za-z]+/s, done\.\n$`)
	if !done.MatchString(out.String()) {
		t.Errorf("output %q does not end with the done line", out.String())
	}
}

func TestMeterSilentWhenNothingMoved(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	log := NewLogger(&out, false)
	log.SetProgress(true)
	log.Meter("Downloading").Finish()

	if out.Len() != 0 {
		t.Errorf("output for an empty transfer: %q", out.String())
	}
}

func TestMeterTicksOnItsOwn(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	log := NewLogger(&out, false)
	log.SetProgress(true)

	meter := log.newMeter("Downloading", 5*time.Millisecond)
	_, _ = meter.Write(make([]byte, 2048))

	// No more writes: only the ticker can redraw the line.
	deadline := time.Now().Add(2 * time.Second)

	for {
		log.mu.Lock()
		redraws := strings.Count(out.String(), "Downloading: 2.0 KiB |")
		log.mu.Unlock()

		if redraws >= 3 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("ticker did not redraw without writes: %d redraws", redraws)
		}

		time.Sleep(5 * time.Millisecond)
	}

	meter.Finish()
}

func TestWarningStartsAfterMeterLine(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	log := NewLogger(&out, false)
	log.SetProgress(true)

	meter := log.newMeter("Uploading", time.Hour)
	_, _ = meter.Write([]byte("x"))
	meter.redraw()

	log.Warnf("late")

	want := eraseLine + "warning: late\n"
	if !strings.HasSuffix(out.String(), want) {
		t.Errorf("output %q does not end with %q", out.String(), want)
	}

	meter.Finish()
}

func TestMeterShowsPercentOfTotal(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	log := NewLogger(&out, false)
	log.SetProgress(true)

	meter := log.Meter("Uploading")
	meter.AddTotal(4096)

	_, _ = meter.Write(make([]byte, 2048))
	meter.Finish()

	done := regexp.MustCompile(`\x1b\[KUploading: 50% \(2\.0 KiB/4\.0 KiB\) \| [0-9.]+ [A-Za-z]+/s, done\.\n$`)
	if !done.MatchString(out.String()) {
		t.Errorf("output %q does not end with the percent line", out.String())
	}
}

func TestStageLine(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	log := NewLogger(&out, false)
	log.Stage("Creating bundle", "1.0 KiB")

	if out.Len() != 0 {
		t.Errorf("stage printed with progress off: %q", out.String())
	}

	log.SetProgress(true)
	log.Stage("Creating bundle", "1.0 KiB")

	if want := eraseLine + "Creating bundle: 1.0 KiB, done.\n"; out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	cases := map[int64]string{
		0:       "0 bytes",
		1023:    "1023 bytes",
		1024:    "1.0 KiB",
		1536:    "1.5 KiB",
		1 << 20: "1.0 MiB",
		3 << 30: "3.0 GiB",
	}

	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
