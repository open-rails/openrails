package serverfixture_test

import (
	"encoding/json"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
	"io/fs"
	"os"
	"sync"
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
	case "declare-psp":
		var declaration billing.PSPDeclaration
		if err = json.Unmarshal(cmd.Body, &declaration); err == nil {
			local, localErr := srv.Engine().LocalMerchant(mid)
			if localErr != nil {
				err = localErr
			} else {
				result, err = local.DeclarePSP(t.Context(), declaration)
			}
		}
	case "new-directory":
		apps, listErr := srv.AuthKit().ListRemoteApplications(t.Context(), iam.GroupByID(mid.String()), iam.PageRequest{})
		if listErr != nil {
			return answer{Error: listErr.Error()}
		}
		app := apps.Items[0]
		app.ID = ""
		app.Issuer = cmd.ID
		app, err = srv.AuthKit().UpsertRemoteApplication(t.Context(), iam.SystemIdentity(), iam.GroupByID(mid.String()), app)
		if err == nil {
			owner, _ := server.MerchantRole("owner")
			result, err = srv.AuthKit().CreateAPIKey(t.Context(), iam.SystemIdentity(), iam.GroupByID(mid.String()), iam.NewAPIKey{Name: "other issuer", Role: owner, ProvisionsFor: app.ID})
		}
	case "revoke-directory-key":
		err = srv.AuthKit().RevokeAPIKey(t.Context(), iam.SystemIdentity(), iam.GroupByID(mid.String()), cmd.ID)
		result = true
	case "host-membership":
		role, _ := server.MerchantRole("owner")
		_, err = srv.AuthKit().SetGroupRole(t.Context(), iam.SystemIdentity(), iam.GroupByID(mid.String()), iam.UserSubject(cmd.ID), role)
		result = true
	case "host-key":
		role, _ := server.MerchantRole("owner")
		result, err = srv.AuthKit().CreateAPIKey(t.Context(), iam.SystemIdentity(), iam.GroupByID(mid.String()), iam.NewAPIKey{Name: "lifecycle", Role: role})
	case "host-concurrent-renames":
		var requests []struct {
			Merchant billing.MerchantID
			Name     string
		}
		if err = json.Unmarshal(cmd.Body, &requests); err != nil {
			break
		}
		outcomes := make([]answer, len(requests))
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for i, request := range requests {
			wg.Add(1)
			go func(i int, mid billing.MerchantID, name string) {
				defer wg.Done()
				<-gate
				value, e := srv.RenameMerchant(t.Context(), mid, billing.RenameMerchantParams{Name: name})
				outcomes[i] = answer{Result: value}
				if e != nil {
					outcomes[i].Error = e.Error()
				}
			}(i, request.Merchant, request.Name)
		}
		close(gate)
		wg.Wait()
		result = outcomes
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
		p.ActorUserID = cmd.Account
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
