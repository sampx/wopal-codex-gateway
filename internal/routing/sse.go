package routing

import (
	"bufio"
	"io"
	"strings"
)

// ReadSSE dispatches each server-sent event's data payload to onData.
func ReadSSE(r io.Reader, onData func([]byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var lines []string
	flush := func() error {
		if len(lines) == 0 {
			return nil
		}
		parts := make([]string, 0, len(lines))
		for _, line := range lines {
			if strings.HasPrefix(line, "data:") {
				parts = append(parts, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		lines = lines[:0]
		if len(parts) == 0 {
			return nil
		}
		return onData([]byte(strings.Join(parts, "\n")))
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}
