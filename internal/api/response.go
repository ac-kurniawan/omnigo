package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var errResponseCommitted = errors.New("response already committed")

type bufferedResponseWriter struct {
	destination   http.ResponseWriter
	header        http.Header
	body          bytes.Buffer
	status        int
	stream        bool
	headerWritten bool
	committed     bool
}

func newBufferedResponseWriter(destination http.ResponseWriter, stream bool) *bufferedResponseWriter {
	return &bufferedResponseWriter{destination: destination, header: make(http.Header), status: http.StatusOK, stream: stream}
}

func (w *bufferedResponseWriter) Header() http.Header {
	return w.header
}

func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.committed || w.headerWritten {
		return
	}
	w.status = status
	w.headerWritten = true
}

func (w *bufferedResponseWriter) Write(data []byte) (int, error) {
	if w.committed {
		return w.destination.Write(data)
	}
	if !w.headerWritten {
		w.headerWritten = true
	}
	return w.body.Write(data)
}

func (w *bufferedResponseWriter) Flush() {
	if !w.stream || w.committed || w.status < http.StatusOK || w.status >= http.StatusMultipleChoices || w.body.Len() == 0 {
		return
	}
	w.commit()
	if flusher, ok := w.destination.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *bufferedResponseWriter) commit() {
	if w.committed {
		return
	}
	copyHeader(w.destination.Header(), w.header)
	w.destination.WriteHeader(w.status)
	w.committed = true
	_, _ = w.destination.Write(w.body.Bytes())
	w.body.Reset()
}

func (w *bufferedResponseWriter) finish(err error) error {
	if w.committed {
		if err != nil {
			return errors.Join(errResponseCommitted, err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if w.status < http.StatusOK || w.status >= http.StatusMultipleChoices {
		return fmt.Errorf("upstream status %d", w.status)
	}
	if w.body.Len() == 0 {
		return errors.New("upstream returned an empty response")
	}
	if !w.stream && strings.Contains(w.header.Get("Content-Type"), "application/json") && !json.Valid(w.body.Bytes()) {
		return errors.New("upstream returned malformed JSON")
	}
	w.commit()
	return nil
}

func copyHeader(destination, source http.Header) {
	for key := range destination {
		destination.Del(key)
	}
	for key, values := range source {
		destination[key] = append([]string(nil), values...)
	}
}
