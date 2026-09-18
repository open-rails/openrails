package inprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// Archive downloads must not collect the entire book in bufferedResponse. This
// bridge preserves the same handler while streaming its body through a pipe.
func streamInprocessResponse(handler http.Handler, req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	req = req.Clone(ctx)
	reader, writer := io.Pipe()
	stop := context.AfterFunc(ctx, func() { _ = writer.CloseWithError(ctx.Err()) })
	body := &streamBody{PipeReader: reader, cancel: cancel}
	headers := make(chan *http.Response, 1)
	w := &streamResponseWriter{header: make(http.Header), writer: writer, reader: body, req: req, headers: headers}
	go func() {
		defer stop()
		if req.Body != nil {
			defer req.Body.Close()
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				_ = writer.CloseWithError(errors.New("in-process archive response interrupted"))
				// Wake a caller if the handler failed before producing headers.
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = writer.Close()
		}()
		handler.ServeHTTP(w, req)
	}()
	select {
	case response := <-headers:
		return response, nil
	case <-req.Context().Done():
		_ = reader.CloseWithError(req.Context().Err())
		_ = writer.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
}

type streamResponseWriter struct {
	header  http.Header
	writer  *io.PipeWriter
	reader  io.ReadCloser
	req     *http.Request
	headers chan<- *http.Response
	wrote   bool
}

func (w *streamResponseWriter) Header() http.Header { return w.header }

func (w *streamResponseWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	header := w.header.Clone()
	length := int64(-1)
	if value := header.Get("Content-Length"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			length = parsed
		}
	}
	w.headers <- &http.Response{
		Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), StatusCode: status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: header, Body: w.reader, ContentLength: length, Request: w.req,
	}
}

func (w *streamResponseWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.writer.Write(p)
}

// Closing an unread download must release both the producer and its DB work.
type streamBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (b *streamBody) Close() error {
	err := b.PipeReader.Close()
	b.cancel()
	return err
}
