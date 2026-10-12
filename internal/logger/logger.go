// Package logger provides a small leveled logger built on the standard
// library log package. It writes timestamped lines to stderr and, optionally,
// to a daily-rotated log file. The console and the file each have their own
// minimum level.
//
// The package-level functions (Debugf/Infof/Warnf/Errorf, Setup, Reload,
// Close) operate on a process-global logger.
package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Level is the logging severity.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var levelNames = map[Level]string{
	LevelDebug: "DEBUG",
	LevelInfo:  "INFO",
	LevelWarn:  "WARN",
	LevelError: "ERROR",
}

// logFileMode is the permission of log files: lines may contain account
// identifiers, so they are private to the service user.
const logFileMode = 0o600

// parseLevel parses a level string (case-insensitive). Unknown values default
// to INFO.
func parseLevel(s string) Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return LevelDebug
	case "WARN", "WARNING":
		return LevelWarn
	case "ERROR":
		return LevelError
	default:
		return LevelInfo
	}
}

// Logger is a leveled logger with a console sink and an optional file sink.
type Logger struct {
	mu        sync.Mutex
	level     Level // console minimum level
	fileLevel Level // file minimum level, independent of level
	stdLogger *log.Logger
	file      *os.File
	maxFiles  int
	fileDir   string
	fileDay   string
}

var defaultLogger = newLogger(os.Stderr)

// newLogger creates a Logger whose console sink is w. The log line already
// carries its own timestamp, so the underlying log.Logger adds none.
func newLogger(w io.Writer) *Logger {
	return &Logger{
		level:     LevelInfo,
		fileLevel: LevelInfo,
		stdLogger: log.New(w, "", 0),
	}
}

// Setup configures the global logger level and optional file logging.
func Setup(level string, fileLogging bool, fileDir string, maxFiles int) {
	defaultLogger.setup(level, fileLogging, fileDir, maxFiles)
}

// Reload re-applies level / file settings without re-creating the global logger.
// An empty fileLevel or a non-positive maxFiles leaves that setting unchanged.
func Reload(level string, fileLevel string, maxFiles int) {
	defaultLogger.reload(level, fileLevel, maxFiles)
}

// Close flushes and closes the global logger's log file. Later log calls keep
// going to the console only; Setup can attach a file again.
func Close() error { return defaultLogger.Close() }

func (l *Logger) setup(level string, fileLogging bool, fileDir string, maxFiles int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = parseLevel(level)
	l.fileDir = fileDir
	l.maxFiles = maxFiles
	if l.maxFiles <= 0 {
		l.maxFiles = 7
	}
	if fileLogging && fileDir != "" {
		if err := os.MkdirAll(fileDir, 0o755); err == nil {
			l.openFileLocked()
		}
	}
}

func (l *Logger) reload(level string, fileLevel string, maxFiles int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = parseLevel(level)
	if fileLevel != "" {
		l.fileLevel = parseLevel(fileLevel)
	}
	if maxFiles > 0 {
		l.maxFiles = maxFiles
	}
}

// Close syncs and closes the log file (if any). It is safe to call twice.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	serr := l.file.Sync()
	cerr := l.file.Close()
	l.file = nil
	l.fileDay = ""
	if serr != nil {
		return serr
	}
	return cerr
}

func (l *Logger) openFileLocked() {
	if l.fileDir == "" {
		return
	}
	day := time.Now().Format("2006-01-02")
	if l.file != nil && l.fileDay == day {
		return
	}
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	path := filepath.Join(l.fileDir, "grok2api."+day+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, logFileMode)
	if err != nil {
		return
	}
	// The mode argument only applies to newly created files; tighten files
	// left behind by older versions (0644).
	_ = f.Chmod(logFileMode)
	l.file = f
	l.fileDay = day
	l.rotateLocked()
}

func (l *Logger) rotateLocked() {
	if l.fileDir == "" || l.maxFiles <= 0 {
		return
	}
	entries, err := os.ReadDir(l.fileDir)
	if err != nil {
		return
	}
	type fi struct {
		name string
		mt   time.Time
	}
	var files []fi
	for _, e := range entries {
		n := e.Name()
		if !matchLogName(n) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fi{n, info.ModTime()})
	}
	for len(files) > l.maxFiles {
		var oldest fi
		var oldestIdx = -1
		for i, f := range files {
			if oldestIdx == -1 || f.mt.Before(oldest.mt) {
				oldest = f
				oldestIdx = i
			}
		}
		if oldestIdx < 0 {
			break
		}
		_ = os.Remove(filepath.Join(l.fileDir, oldest.name))
		files = append(files[:oldestIdx], files[oldestIdx+1:]...)
	}
}

func matchLogName(name string) bool {
	if len(name) < 9 || name[:9] != "grok2api." || len(name) < 14 {
		return false
	}
	if name[len(name)-4:] != ".log" {
		return false
	}
	return true
}

// logf formats and routes one line. The console and the file are filtered by
// their own levels, so e.g. a DEBUG file log works with an INFO console.
func (l *Logger) logf(lvl Level, format string, args ...any) {
	// levels are written by setup/reload under l.mu, so read them under it too.
	l.mu.Lock()
	toConsole := lvl >= l.level
	toFile := l.file != nil && lvl >= l.fileLevel
	l.mu.Unlock()
	if !toConsole && !toFile {
		return
	}
	line := fmt.Sprintf("%s [%s] %s", time.Now().Format("2006-01-02 15:04:05.000"), levelNames[lvl], fmt.Sprintf(format, args...))

	l.mu.Lock()
	defer l.mu.Unlock()
	if toConsole {
		l.stdLogger.Println(line)
	}
	// Re-check l.file: Close may have run since the first look.
	if toFile && l.file != nil {
		l.openFileLocked() // daily rotation
		if l.file != nil {
			_, _ = io.WriteString(l.file, line+"\n")
		}
	}
}

func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(LevelInfo, format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(LevelWarn, format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args...) }

// Package-level convenience functions using the default logger.
func Debugf(format string, args ...any) { defaultLogger.Debugf(format, args...) }
func Infof(format string, args ...any)  { defaultLogger.Infof(format, args...) }
func Warnf(format string, args ...any)  { defaultLogger.Warnf(format, args...) }
func Errorf(format string, args ...any) { defaultLogger.Errorf(format, args...) }
