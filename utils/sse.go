package utils

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// SSEEvent represents a parsed Server-Sent Event
type SSEEvent struct {
	Type  string
	Data  string
}

// ParseSSEEvents reads an SSE stream and yields parsed events via channel
func ParseSSEEvents(reader io.Reader) <-chan SSEEvent {
	ch := make(chan SSEEvent, 64)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(reader)
		// Increase buffer size for large events
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

		var eventType, data string
		for scanner.Scan() {
			line := scanner.Text()

			if line == "" {
				// Empty line = end of event
				if data != "" {
					ch <- SSEEvent{Type: eventType, Data: data}
				}
				eventType = ""
				data = ""
				continue
			}

			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				payload := strings.TrimPrefix(line, "data: ")
				if data != "" {
					data += "\n"
				}
				data += payload
			}
		}
		// Flush last event if stream ends without trailing blank line
		if data != "" {
			ch <- SSEEvent{Type: eventType, Data: data}
		}
	}()
	return ch
}

// JSONToReader marshals data to an io.Reader
func JSONToReader(v interface{}) (io.Reader, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("json marshal: %w", err)
	}
	return bytes.NewReader(b), nil
}
