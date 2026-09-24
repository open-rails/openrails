package collection

import "testing"

// The charge path records nmi_response_<code> and nmi_<localization id>; a known
// retry in either shape is a decided retry, not an unmapped code.
func TestPrefixedNMIRetryCodesAreKnown(t *testing.T) {
	for _, code := range []string{"202", "nmi_response_202", "insufficient_funds", "nmi_insufficient_funds"} {
		c := ClassifyDeclineDetail("nmi", code)
		if c.Outcome != DeclineRetry || c.Coverage != CoverageKnownRetry || c.NeedsMapping() {
			t.Errorf("%s: outcome %v coverage %v", code, c.Outcome, c.Coverage)
		}
	}
}
