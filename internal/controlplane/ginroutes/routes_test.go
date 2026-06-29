package ginroutes

import "testing"

func TestToGinPath(t *testing.T) {
	cases := map[string]string{
		"/token":                                 "/token",
		"/owners/{slug}":                         "/owners/:slug",
		"/user/sessions/{id}":                    "/user/sessions/:id",
		"/merchant/{merchant}/members/{user_id}": "/merchant/:merchant/members/:user_id",
		"/user/providers/{provider}":             "/user/providers/:provider",
		"":                                       "",
	}
	for in, want := range cases {
		if got := toGinPath(in); got != want {
			t.Errorf("toGinPath(%q) = %q, want %q", in, got, want)
		}
	}
}
