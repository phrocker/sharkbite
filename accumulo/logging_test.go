package accumulo

import "testing"

func TestClientLogLevelIsProcessWideAndValidated(t *testing.T) {
	t.Cleanup(func() {
		SetLogLevel(LogOff)
		SetLogSink(nil)
	})
	var events []LogEvent
	SetLogSink(func(event LogEvent) { events = append(events, event) })
	SetLogLevel(LogDebug)
	if got := CurrentLogLevel(); got != LogDebug {
		t.Fatalf("log level = %v, want debug", got)
	}
	SetLogLevel(LogTrace)
	if got := CurrentLogLevel(); got != LogTrace {
		t.Fatalf("log level = %v, want trace", got)
	}
	SetLogLevel(LogLevel(99))
	if got := CurrentLogLevel(); got != LogOff {
		t.Fatalf("invalid log level = %v, want off", got)
	}
	if len(events) != 2 ||
		events[0].Message != "shoal.logging.level_changed" ||
		events[0].Attributes["level"] != "debug" {
		t.Fatalf("structured events = %+v", events)
	}
	if _, leaked := events[0].Attributes["password"]; leaked {
		t.Fatal("structured log contains a credential field")
	}
}
