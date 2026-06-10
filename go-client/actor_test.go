package openrails

import "testing"

func TestActorFor(t *testing.T) {
	cases := []struct {
		name string
		in   ActorInputs
		want string
	}{
		{
			name: "service token uses key id",
			in:   ActorInputs{Source: "service_token", ServiceTokenKeyID: "k1", Fallback: "acme"},
			want: "service-token:k1",
		},
		{
			name: "delegated jwt uses issuer:sub",
			in:   ActorInputs{Source: "platform_delegated_jwt", DelegatedIssuerID: "iss", DelegatedSub: "sub-9", Fallback: "acme"},
			want: "iss:sub-9",
		},
		{
			name: "direct user id",
			in:   ActorInputs{Source: "authkit", UserID: "u123", Fallback: "acme"},
			want: "user:u123",
		},
		{
			name: "subject fallback for user",
			in:   ActorInputs{Source: "authkit", Subject: "subj", Fallback: "acme"},
			want: "user:subj",
		},
		{
			name: "fallback when nothing finer",
			in:   ActorInputs{Source: "api_key", Fallback: "acme"},
			want: "acme",
		},
		{
			name: "service token without key id falls back",
			in:   ActorInputs{Source: "service_token", Fallback: "acme"},
			want: "acme",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ActorFor(tc.in); got != tc.want {
				t.Fatalf("ActorFor = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServiceTokenKeyIDFromToken(t *testing.T) {
	cases := map[string]string{
		"cozy_st_k1_thesecret":    "k1",
		"cozy_st_abc123_s_e_cret": "abc123",
		"not_a_service_token":     "",
		"cozy_st_nounderscore":    "",
		"":                        "",
	}
	for token, want := range cases {
		if got := ServiceTokenKeyIDFromToken(token); got != want {
			t.Fatalf("ServiceTokenKeyIDFromToken(%q) = %q, want %q", token, got, want)
		}
	}
}
