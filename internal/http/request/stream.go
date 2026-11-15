package request

import "io"

// Stream writes a prepared binary or line-oriented response without buffering
// it into a JSON value. Set response headers before calling Stream.
func (r *Request) Stream(code int, src io.Reader) error {
	r.w.WriteHeader(code)
	_, err := io.Copy(r.w, src)
	return err
}
