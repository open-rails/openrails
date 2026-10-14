package pspimport

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
)

// The CCBill pull reads a real account's DataLink in test mode: set
// OPENRAILS_TEST_CCBILL_ACCOUNT (clientAccnum-clientSubacc),
// OPENRAILS_TEST_CCBILL_DATALINK_USERNAME and _PASSWORD.
func TestCCBillPullReadsDataLink(t *testing.T) {
	account, user, pass := os.Getenv("OPENRAILS_TEST_CCBILL_ACCOUNT"), os.Getenv("OPENRAILS_TEST_CCBILL_DATALINK_USERNAME"), os.Getenv("OPENRAILS_TEST_CCBILL_DATALINK_PASSWORD")
	if account == "" || user == "" || pass == "" {
		t.Skip("no CCBill DataLink test credentials")
	}
	acc, sub, ok := strings.Cut(account, "-")
	require.True(t, ok)
	dl := ccbill.NewDataLinkClient(&config.CCBillConfig{ClientAccNum: acc, ClientSubAcc: sub, DataLinkUsername: user, DataLinkPassword: pass, Sandbox: true})
	dl.ReadOnly = true
	src := &ccbillSource{dl: dl}
	roster, err := src.roster(t.Context())
	require.NoError(t, err)
	until := time.Now().UTC()
	history, err := src.history(t.Context(), until.Add(-48*time.Hour), until)
	require.NoError(t, err)
	t.Logf("staged %d members; %d rebills in two days", len(roster), len(history))
}
