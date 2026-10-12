package hostedcheckout

import "testing"

func TestOriginRefusesEquivalentApplicationOrigins(t *testing.T) {
	for _, pair := range [][2]string{
		{"https://identity.example:443", "https://identity.example"},
		{"https://identity.example:0443", "https://identity.example"},
		{"http://[0:0:0:0:0:0:0:1]", "http://[::1]"},
		{"https://127.1", "https://127.0.0.1"},
		{"https://0x7f000001", "https://127.0.0.1"},
		{"https://2130706433", "https://127.0.0.1"},
		{"https://identity.example", "https://IDENTITY.example:443/auth"},
		{"http://localhost:80", "http://localhost"},
		{"http://localhost", "http://localhost:80/auth"},
		{"https://bücher.example", "https://xn--bcher-kva.example"},
	} {
		if origin, err := Origin(pair[0], pair[1]); err == nil {
			t.Errorf("accepted shared origin %s against %s: %s", pair[0], pair[1], origin)
		}
	}
}
func TestOriginKeepsNondefaultPortsDistinct(t *testing.T) {
	origin, err := Origin("https://pay.example:443/", "https://pay.example:444")
	if err != nil || origin != "https://pay.example" {
		t.Fatalf("origin=%s err=%v", origin, err)
	}
}
