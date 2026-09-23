package requestauth

import (
	"errors"
	"net/http/httptest"
	"testing"
)

func TestOnceCachesNilInterfaceFailure(t *testing.T) {
	ctx := Begin(httptest.NewRequest("GET", "/", nil)).Context()
	owner := new(int)
	calls := 0
	denied := errors.New("credential denied")
	for range 2 {
		value, err := Once(ctx, owner, func() (any, error) { calls++; return nil, denied })
		if value != nil || !errors.Is(err, denied) {
			t.Fatalf("cached failure = %v, %v", value, err)
		}
	}
	if calls != 1 {
		t.Fatalf("verification calls = %d", calls)
	}
}
