package client

import (
	"bufio"
	"io"
	"strings"
)

// sseReader reads Server-Sent Events: the ports watch and the event stream.
type sseReader struct {
	body io.ReadCloser
	sc   *bufio.Scanner
}

func newSSE(body io.ReadCloser) *sseReader {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	return &sseReader{body: body, sc: sc}
}

// next returns the next event's name and data. A stream that ends cleanly is io.EOF.
func (r *sseReader) next() (event, data string, err error) {
	var lines []string
	for r.sc.Scan() {
		line := r.sc.Text()
		if line == "" {
			if lines == nil && event == "" {
				continue
			}
			if event == "" {
				event = "message"
			}
			return event, strings.Join(lines, "\n"), nil
		}
		if strings.HasPrefix(line, ":") {
			continue // a comment: keep-alives
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			lines = append(lines, value)
		}
	}
	if err := r.sc.Err(); err != nil {
		return "", "", err
	}
	return "", "", io.EOF
}

func (r *sseReader) Close() error { return r.body.Close() }
