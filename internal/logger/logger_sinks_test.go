package logger

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func nowDay() string { return time.Now().Format("2006-01-02") }

func readLogFile(t *testing.T, dir string) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "grok2api.*.log"))
	if len(matches) != 1 {
		t.Fatalf("expected one log file in %s, got %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stderr lines used to carry two timestamps (log.LstdFlags + our own).
func TestConsoleLineHasSingleTimestamp(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf)
	l.Infof("hello %s", "world")
	re := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3} \[INFO\] hello world\n$`)
	if !re.MatchString(buf.String()) {
		t.Fatalf("unexpected console line: %q", buf.String())
	}
}

// The file honours its own level, independent of the console level (the
// effective file level used to be max(console, file)).
func TestFileLevelIndependentOfConsoleLevel(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer
	l := newLogger(&console)
	l.setup("ERROR", true, dir, 7)
	l.reload("ERROR", "DEBUG", 0)
	t.Cleanup(func() { _ = l.Close() })

	l.Debugf("only-in-file")
	l.Errorf("in-both")
	if strings.Contains(console.String(), "only-in-file") {
		t.Error("console must not show DEBUG when its level is ERROR")
	}
	if !strings.Contains(console.String(), "in-both") {
		t.Error("console must show ERROR")
	}
	got := readLogFile(t, dir)
	if !strings.Contains(got, "only-in-file") || !strings.Contains(got, "in-both") {
		t.Fatalf("file must hold DEBUG and ERROR lines, got %q", got)
	}
}

func TestConsoleLevelIndependentOfFileLevel(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer
	l := newLogger(&console)
	l.setup("DEBUG", true, dir, 7)
	l.reload("DEBUG", "ERROR", 0)
	t.Cleanup(func() { _ = l.Close() })

	l.Infof("console-only")
	l.Errorf("everywhere")
	if !strings.Contains(console.String(), "console-only") {
		t.Error("console must show INFO when its level is DEBUG")
	}
	got := readLogFile(t, dir)
	if strings.Contains(got, "console-only") || !strings.Contains(got, "everywhere") {
		t.Fatalf("file must only hold ERROR lines, got %q", got)
	}
}

func TestLogFileIs0600(t *testing.T) {
	dir := t.TempDir()
	// A file left behind by an older version with 0644 gets tightened.
	old := filepath.Join(dir, "grok2api."+nowDay()+".log")
	if err := os.WriteFile(old, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := newLogger(&bytes.Buffer{})
	l.setup("INFO", true, dir, 7)
	t.Cleanup(func() { _ = l.Close() })
	l.Infof("x")
	fi, err := os.Stat(old)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("log file mode = %v, want 0600", fi.Mode().Perm())
	}

	dir2 := t.TempDir()
	l2 := newLogger(&bytes.Buffer{})
	l2.setup("INFO", true, dir2, 7)
	t.Cleanup(func() { _ = l2.Close() })
	l2.Infof("y")
	matches, _ := filepath.Glob(filepath.Join(dir2, "*.log"))
	if len(matches) != 1 {
		t.Fatalf("matches = %v", matches)
	}
	fi, _ = os.Stat(matches[0])
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("new log file mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestCloseStopsFileLoggingAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer
	l := newLogger(&console)
	l.setup("INFO", true, dir, 7)
	l.Infof("before-close")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	l.Infof("after-close")
	if !strings.Contains(console.String(), "after-close") {
		t.Error("console logging must continue after Close")
	}
	got := readLogFile(t, dir)
	if !strings.Contains(got, "before-close") {
		t.Errorf("flushed content missing: %q", got)
	}
	if strings.Contains(got, "after-close") {
		t.Errorf("file reopened or written after Close: %q", got)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug": LevelDebug, "DEBUG": LevelDebug, " Warn ": LevelWarn, "WARNING": LevelWarn,
		"error": LevelError, "info": LevelInfo, "": LevelInfo, "bogus": LevelInfo,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}
