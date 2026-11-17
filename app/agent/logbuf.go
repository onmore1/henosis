package agent

import (
	"fmt"
	"sync"
	"time"
)

// LogEntry is one line of the debug console.
type LogEntry struct {
	Seq   int64  `json:"seq"`
	Time  string `json:"time"`  // local time, HH:MM:SS
	Level string `json:"level"` // info, error or debug
	Text  string `json:"text"`
}

// LogBuffer keeps the most recent log lines for the debug console.
type LogBuffer struct {
	mu      sync.Mutex
	entries []LogEntry
	seq     int64
	limit   int
}

// NewLogBuffer makes a buffer that keeps the last limit lines.
func NewLogBuffer(limit int) *LogBuffer { return &LogBuffer{limit: limit} }

// Add appends a line and returns it.
func (b *LogBuffer) Add(level, format string, args ...any) LogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	entry := LogEntry{Seq: b.seq, Time: time.Now().Format("15:04:05"), Level: level, Text: fmt.Sprintf(format, args...)}
	b.entries = append(b.entries, entry)
	if len(b.entries) > b.limit {
		b.entries = append([]LogEntry(nil), b.entries[len(b.entries)-b.limit:]...)
	}
	return entry
}

// After returns the lines newer than seq.
func (b *LogBuffer) After(seq int64) []LogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []LogEntry{}
	for _, entry := range b.entries {
		if entry.Seq > seq {
			out = append(out, entry)
		}
	}
	return out
}
