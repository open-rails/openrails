//go:build integration

package nmi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TestLiveCollectJSTokenVaultCreate closes the LAST unverified seam of the
// #663 v5 migration: a REAL Collect.js token (minted by NMI's own browser
// script, exactly like production checkout) consumed by OUR
// CreateCustomerVault via v5 POST /customers with
// billing.payment_details.payment_token.
//
// Opt-in: needs NMI_SANDBOX_SECURITY_KEY + NMI_TOKENIZATION_KEY and a Chrome
// binary. Headless Chrome runs with --disable-web-security so the harness
// page can fill Collect.js's cross-origin card-field iframes directly — the
// tokenization itself is untouched NMI production code.
func TestLiveCollectJSTokenVaultCreate(t *testing.T) {
	securityKey := strings.TrimSpace(os.Getenv("NMI_SANDBOX_SECURITY_KEY"))
	tokenizationKey := strings.TrimSpace(os.Getenv("NMI_TOKENIZATION_KEY"))
	if securityKey == "" || tokenizationKey == "" {
		t.Skip("NMI_SANDBOX_SECURITY_KEY / NMI_TOKENIZATION_KEY not set; skipping live Collect.js proof")
	}
	chrome := findChrome()
	if chrome == "" {
		t.Skip("no Chrome/Chromium binary found; skipping live Collect.js proof")
	}
	collectURL := strings.TrimSpace(os.Getenv("NMI_TOKENIZATION_URL"))
	if collectURL == "" {
		collectURL = "https://secure.nmi.com/token/Collect.js"
	}

	tokenCh := make(chan string, 1)
	logCh := make(chan string, 64)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, collectJSHarnessHTML(collectURL, tokenizationKey))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		select {
		case tokenCh <- strings.TrimSpace(string(body)):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/log", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		select {
		case logCh <- string(body):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	profileDir := t.TempDir()
	cmd := exec.CommandContext(ctx, chrome,
		"--headless=new", "--no-sandbox", "--disable-gpu",
		// Let the harness fill the cross-origin card iframes: web security off
		// AND site isolation off (isolated frames have a nil contentDocument
		// from the parent regardless of web security).
		"--disable-web-security",
		"--disable-site-isolation-trials",
		"--disable-features=IsolateOrigins,site-per-process",
		"--user-data-dir="+profileDir,
		"--no-first-run", "--disable-extensions", "--mute-audio",
		server.URL)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	var token string
	deadline := time.After(75 * time.Second)
waitToken:
	for {
		select {
		case token = <-tokenCh:
			break waitToken
		case line := <-logCh:
			t.Logf("harness: %s", line)
		case <-deadline:
			t.Fatal("timed out waiting for a Collect.js payment token from headless Chrome")
		}
	}
	require.NotEmpty(t, token, "harness must deliver a non-empty payment token")
	t.Logf("got real Collect.js token: %s...", token[:min(12, len(token))])

	// The seam under test: OUR client consumes the real token on the LIVE v5
	// gateway — the exact production vault-creation wire shape.
	client, err := NewClient("live-collectjs", &config.NMIProviderSettings{SecurityKey: securityKey}, true)
	require.NoError(t, err)
	require.Equal(t, DefaultV5BaseURL, client.V5BaseURL, "must hit the real v5 gateway, not a stub")

	created, err := client.CreateCustomerVault(ctx, CreateCustomerVaultData{
		PaymentToken: token,
		FirstName:    "CollectJS",
		LastName:     "LiveProof",
		Zip:          "60001",
	})
	require.NoError(t, err, "v5 create-customer must accept a real Collect.js token in billing.payment_details.payment_token")
	require.NotEmpty(t, created.CustomerVaultID)
	require.NotEmpty(t, created.BillingID, "the token-minted billing entry id must be returned (#682)")
	t.Cleanup(func() {
		_ = client.DeleteCustomerVault(ctx, DeleteCustomerVaultData{CustomerVaultID: created.CustomerVaultID})
	})

	// Read back: the vault holds the tokenized test card.
	page, err := client.ListCustomersPage(ctx, "", 5, created.CustomerVaultID)
	require.NoError(t, err)
	require.Len(t, page.Customers, 1)
	require.Len(t, page.Customers[0].Billing, 1)
	require.Equal(t, created.BillingID, page.Customers[0].Billing[0].ID)
	ccnumber := page.Customers[0].Billing[0].PaymentDetails.CardNumber
	require.True(t, strings.HasSuffix(ccnumber, "1111"), "vaulted card must be the tokenized 4111... test card, got %q", ccnumber)

	// And the vault is chargeable — the full production checkout sequence.
	sale, err := client.RunSale(ctx, SaleParams{
		CustomerVaultID:  created.CustomerVaultID,
		Amount:           moneyCentsForCollectProbe(),
		Currency:         "USD",
		OrderDescription: "collectjs live proof",
		OrderID:          fmt.Sprintf("collectjs-%d", time.Now().UnixNano()%1e10),
	})
	require.NoError(t, err, "token-minted vault must be chargeable")
	require.NotEmpty(t, sale.TransactionID)
	require.NoError(t, client.Void(ctx, sale.TransactionID))
}

func findChrome() string {
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

// moneyCentsForCollectProbe randomizes cents to dodge NMI duplicate-transaction
// detection across quick re-runs.
func moneyCentsForCollectProbe() moneyutil.Cents {
	return moneyutil.Cents(105 + time.Now().UnixNano()%80)
}

// collectJSHarnessHTML is the minimal production-shaped Collect.js page: NMI's
// script tokenizes; the page (running with web security disabled) types the
// test card into the field iframes and posts the resulting token home.
// Live-verified quirk: the token flows via the paymentSelector button click;
// a bare CollectJS.startPaymentRequest() with no paymentSelector configured
// stalled silently (validation passed, no callback) — keep the button.
func collectJSHarnessHTML(collectURL, tokenizationKey string) string {
	return `<!DOCTYPE html><html><head>
<script>
function say(m){fetch('/log',{method:'POST',body:String(m)}).catch(function(){});}
window.onerror=function(m,s,l){say('pageerror: '+m+' @'+s+':'+l);};
['log','warn','error'].forEach(function(k){var o=console[k].bind(console);console[k]=function(){try{say('console.'+k+': '+Array.prototype.map.call(arguments,String).join(' '));}catch(e){}o.apply(null,arguments);};});
window.addEventListener('message',function(e){try{say('msg from '+e.origin+': '+String(JSON.stringify(e.data)).slice(0,200));}catch(err){}});
</script>
<script src="` + collectURL + `" data-tokenization-key="` + tokenizationKey + `"></script>
</head><body>
<div id="ccnumber"></div><div id="ccexp"></div><div id="cvv"></div>
<button id="payButton" type="button">Pay</button>
<script>
var fieldsReady={};
function fillFrame(sel,value){
  var f=document.querySelector(sel+' iframe');
  if(!f||!f.contentDocument){say('no frame doc for '+sel);return false;}
  var input=f.contentDocument.querySelector('input');
  if(!input){say('no input in '+sel);return false;}
  input.focus();
  var proto=Object.getOwnPropertyDescriptor(Object.getPrototypeOf(input),'value')
        ||Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value');
  proto.set.call(input,value);
  input.dispatchEvent(new Event('input',{bubbles:true}));
  input.dispatchEvent(new Event('change',{bubbles:true}));
  input.blur();
  return true;
}
function start(){
  CollectJS.configure({
    variant:'inline',
    paymentSelector:'#payButton',
    fields:{
      ccnumber:{selector:'#ccnumber'},
      ccexp:{selector:'#ccexp'},
      cvv:{selector:'#cvv'}
    },
    fieldsAvailableCallback:function(){
      say('fields available');
      setTimeout(function(){
        var ok=fillFrame('#ccnumber','4111111111111111')
             && fillFrame('#ccexp','10/30')
             && fillFrame('#cvv','999');
        say('filled='+ok);
        setTimeout(function(){
          say('requesting token (button click)');
          try{document.getElementById('payButton').click();}catch(e){say('click error: '+e);}
          setTimeout(function(){
            say('fallback startPaymentRequest');
            try{CollectJS.startPaymentRequest();}catch(e){say('start error: '+e);}
          },6000);
        },1500);
      },1000);
    },
    validationCallback:function(field,status,message){
      say('validate '+field+' '+status+' '+message);
    },
    timeoutCallback:function(){say('collect.js timeout');},
    callback:function(response){
      say('token callback');
      fetch('/token',{method:'POST',body:response.token});
    }
  });
}
if(window.CollectJS){start();}else{say('CollectJS missing after script load');}
</script>
</body></html>`
}
