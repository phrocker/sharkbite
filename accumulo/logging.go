package accumulo

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

// LogLevel controls process-wide diagnostics emitted by the Accumulo client.
type LogLevel int32

const (
	LogOff LogLevel = iota
	LogDebug
	LogTrace
)

var clientLogLevel atomic.Int32
var clientLogSink = struct {
	sync.RWMutex
	sink func(LogEvent)
}{sink: defaultLogSink}

// LogEvent is an owned structured client diagnostic. Attributes never contain
// connector passwords, instance secrets, or authentication tokens.
type LogEvent struct {
	Level      LogLevel
	Message    string
	Attributes map[string]string
}

func defaultLogSink(event LogEvent) {
	args := make([]any, 0, len(event.Attributes)*2)
	for key, value := range event.Attributes {
		args = append(args, key, value)
	}
	slog.Debug(event.Message, args...)
}

// SetLogSink replaces the process-wide structured diagnostic sink. Passing nil
// restores slog.Default-backed output.
func SetLogSink(sink func(LogEvent)) {
	if sink == nil {
		sink = defaultLogSink
	}
	clientLogSink.Lock()
	clientLogSink.sink = sink
	clientLogSink.Unlock()
}

// SetLogLevel sets the process-wide Accumulo client diagnostic level.
func SetLogLevel(level LogLevel) {
	if level < LogOff || level > LogTrace {
		level = LogOff
	}
	clientLogLevel.Store(int32(level))
	logClient(LogDebug, "shoal.logging.level_changed", "level", level.String())
}

// CurrentLogLevel returns the process-wide Accumulo client diagnostic level.
func CurrentLogLevel() LogLevel {
	return LogLevel(clientLogLevel.Load())
}

func logClient(level LogLevel, message string, args ...any) {
	if CurrentLogLevel() < level {
		return
	}
	attributes := make(map[string]string, len(args)/2)
	for index := 0; index+1 < len(args); index += 2 {
		key, ok := args[index].(string)
		if !ok {
			continue
		}
		attributes[key] = fmt.Sprint(args[index+1])
	}
	clientLogSink.RLock()
	sink := clientLogSink.sink
	sink(LogEvent{Level: level, Message: message, Attributes: attributes})
	clientLogSink.RUnlock()
}

func (l LogLevel) String() string {
	switch l {
	case LogDebug:
		return "debug"
	case LogTrace:
		return "trace"
	default:
		return "off"
	}
}
