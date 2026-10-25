package serverfixture_test

import (
	"encoding/json"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
	"io/fs"
	"os"
	"testing"
	"time"
)

func hostPaymentAssets(path string) fs.FS {
	if path == "" {
		return nil
	}
	return os.DirFS(path)
}

// Controls call only the public trusted-host API. They are a parent-process
// bridge, not replacement authorization or an alternate business data path.
func hostCommand(t *testing.T, srv *server.Server, clock *clockwork.FakeClock, cmd command) answer {
	var result any
	var err error
	mid, _ := billing.ParseMerchantID(cmd.Merchant)
	switch cmd.Op {
	case "revoke-directory-key":
		err = srv.AuthKit().RevokeAPIKey(t.Context(), iam.SystemIdentity(), iam.GroupByID(mid.String()), cmd.ID)
		result = true
	case "host-provision":
		var p billing.ProvisionMerchantParams
		err = json.Unmarshal(cmd.Body, &p)
		if err == nil {
			result, err = srv.ProvisionMerchant(t.Context(), p)
		}
	case "host-get":
		result, err = srv.GetMerchant(t.Context(), mid)
	case "host-list":
		var p billing.MerchantListParams
		err = json.Unmarshal(cmd.Body, &p)
		if err == nil {
			result, err = srv.ListMerchants(t.Context(), p)
		}
	case "host-find":
		var id billing.MerchantID
		var name string
		id, name, err = srv.MerchantByName(t.Context(), cmd.ID)
		result = map[string]any{"id": id, "name": name}
	case "host-rename":
		var p billing.RenameMerchantParams
		err = json.Unmarshal(cmd.Body, &p)
		if err == nil {
			result, err = srv.RenameMerchant(t.Context(), mid, p)
		}
	case "host-delete":
		result, err = srv.DeleteMerchant(t.Context(), mid)
	case "host-restore":
		result, err = srv.RestoreMerchant(t.Context(), mid)
	case "host-retire":
		result, err = srv.RetireUnusedMerchant(t.Context(), mid, cmd.ID)
	case "host-complete-retirements":
		result, err = srv.CompletePendingMerchantRetirements(t.Context(), 100)
	case "host-memberships":
		result, err = srv.ListUserMerchants(t.Context(), cmd.ID)
	case "advance":
		var duration time.Duration
		duration, err = time.ParseDuration(cmd.ID)
		if err == nil {
			clock.Advance(duration)
			result = clock.Now()
		}
	default:
		return answer{Error: "unknown fixture command"}
	}
	if err != nil {
		return answer{Error: err.Error()}
	}
	return answer{Result: result}
}
