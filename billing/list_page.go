package billing

import "encoding/json"

// DefaultPageLimit and MaxPageLimit bound PageRequest.Limit.
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 500
)

// PageRequest asks for one page of a list: at most Limit items after Cursor.
// A zero Limit is DefaultPageLimit; an empty Cursor is the first page.
type PageRequest struct {
	Limit  int
	Cursor string
}

// ListPage is one page of a list, in the list's own order. Next is the opaque
// cursor of the following page; "" marks the last page.
//
// On the wire it is {"data": [...], "next_cursor": string|null}: the one list
// envelope. data is never null.
type ListPage[T any] struct {
	Items []T
	Next  string
}

type listPageJSON[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

func (p ListPage[T]) MarshalJSON() ([]byte, error) {
	out := listPageJSON[T]{Data: p.Items}
	if out.Data == nil {
		out.Data = []T{}
	}
	if p.Next != "" {
		out.NextCursor = &p.Next
	}
	return json.Marshal(out)
}

func (p *ListPage[T]) UnmarshalJSON(raw []byte) error {
	var in listPageJSON[T]
	if err := json.Unmarshal(raw, &in); err != nil {
		return err
	}
	p.Items, p.Next = in.Data, ""
	if in.NextCursor != nil {
		p.Next = *in.NextCursor
	}
	return nil
}
