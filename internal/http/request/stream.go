package request

import (
	"fmt"
	"io"
)

// Stream writes a prepared binary or line-oriented response without buffering
// it into a JSON value. Set response headers before calling Stream.
func (r *Request) Stream(code int, src io.Reader) error {
	h, ok := r.t.(*httpTransport)
	if !ok {
		return fmt.Errorf("request transport does not support streamed responses")
	}
	h.w.WriteHeader(code)
	_, err := io.Copy(h.w, src)
	return err
}
