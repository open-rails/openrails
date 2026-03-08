package server

import (
	"bytes"
	"html/template"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type debugMobiusTokenizationPageData struct {
	Provider            string
	Mode                string
	TokenizationKey     string
	TokenizationURL     string
	EffectiveScriptURL  string
	EffectiveKeyHint    string
	HasTokenizationKey  bool
	HasTokenizationURL  bool
	IsConfiguredForReal bool
}

var debugMobiusTokenizationTemplate = template.Must(template.New("debug_mobius_tokenization").Parse(`<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>Billing Debug: Mobius Tokenization</title>
    <style>
      body { font-family: ui-sans-serif, system-ui, -apple-system, Segoe UI, Roboto, Helvetica, Arial; padding: 24px; max-width: 920px; margin: 0 auto; }
      code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", "Courier New", monospace; }
      .row { display: flex; gap: 12px; flex-wrap: wrap; }
      .card { border: 1px solid #ddd; border-radius: 12px; padding: 16px; margin: 12px 0; }
      .muted { color: #666; }
      label { display: block; margin: 8px 0 4px; font-weight: 600; }
      input, textarea, select { width: 100%; padding: 10px; border-radius: 10px; border: 1px solid #ccc; }
      button { padding: 10px 14px; border-radius: 10px; border: 1px solid #333; background: #111; color: #fff; cursor: pointer; }
      button.secondary { background: #fff; color: #111; border-color: #bbb; }
      .danger { color: #a00; }
      .ok { color: #0a0; }
      .two { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
      @media (max-width: 820px) { .two { grid-template-columns: 1fr; } }
    </style>
  </head>
  <body>
    <h1>Billing Debug: Mobius/NMI tokenization</h1>
    <p class="muted">Dev-only harness page. This service never sees raw card details; Collect.js generates a <code>payment_token</code> in the browser.</p>

    <div class="card">
      <div class="row">
        <div style="flex:1; min-width: 280px;">
          <div><strong>provider</strong>: <code>{{.Provider}}</code></div>
          <div><strong>mode</strong>: <code>{{.Mode}}</code></div>
          <div><strong>Collect.js</strong>: <code>{{.EffectiveScriptURL}}</code></div>
          <div><strong>tokenization key</strong>: <code>{{.EffectiveKeyHint}}</code></div>
        </div>
        <div style="flex:1; min-width: 280px;">
          <div><a href="?mode=real">real Collect.js</a> · <a href="?mode=stub">local stub</a></div>
          <div class="muted" style="margin-top:8px;">
            Real mode requires <code>processors.mobius.tokenization_key</code> and <code>processors.mobius.tokenization_url</code>.
          </div>
        </div>
      </div>
      {{if not .IsConfiguredForReal}}
      <p class="danger" style="margin-top:12px;">
        Real tokenization is not fully configured (missing tokenization key and/or URL). Stub mode still works for wiring checks.
      </p>
      {{else}}
      <p class="ok" style="margin-top:12px;">Real tokenization appears configured. If tokenization fails, check allowed origins in the NMI/Mobius portal.</p>
      {{end}}
    </div>

    <div class="card">
      <h2>1) Generate <code>payment_token</code></h2>
      <div class="two">
        <div>
          <label for="ccnumber">Card number</label>
          <input id="ccnumber" autocomplete="cc-number" placeholder="4111 1111 1111 1111" />
        </div>
        <div>
          <label for="ccexp">Expiry</label>
          <input id="ccexp" autocomplete="cc-exp" placeholder="10 / 30" />
        </div>
        <div>
          <label for="cvv">CVV</label>
          <input id="cvv" autocomplete="cc-csc" placeholder="123" />
        </div>
        <div>
          <label for="zip">Billing ZIP</label>
          <input id="zip" autocomplete="postal-code" placeholder="77777" value="77777" />
        </div>
      </div>

      <div class="row" style="margin-top: 12px;">
        <button id="btn-config" class="secondary" type="button">Configure Collect.js</button>
        <button id="btn-token" type="button">Generate token</button>
      </div>

      <label for="token" style="margin-top:12px;">Token output</label>
      <textarea id="token" rows="3" readonly placeholder="payment_token will appear here"></textarea>
      <div class="row" style="margin-top: 8px;">
        <button id="btn-copy-token" class="secondary" type="button">Copy token</button>
      </div>
      <div id="status" class="muted" style="margin-top:8px;"></div>
    </div>

    <div class="card">
      <h2>2) (Optional) Call billing with the token</h2>
      <p class="muted">This calls <code>POST /v1/me/payment-methods</code> and requires a valid JWT for a user.</p>

      <label for="jwt">Bearer token (JWT)</label>
      <textarea id="jwt" rows="2" placeholder="paste JWT here (no 'Bearer ' prefix)"></textarea>

      <label for="e2e_run_id">E2E Run ID (optional)</label>
      <input id="e2e_run_id" placeholder="e2e_20250101T000000_..." />

      <div class="two">
        <div>
          <label for="first_name">First name</label>
          <input id="first_name" value="Test" />
        </div>
        <div>
          <label for="last_name">Last name</label>
          <input id="last_name" value="User" />
        </div>
        <div>
          <label for="address1">Address</label>
          <input id="address1" value="888 Test St" />
        </div>
        <div>
          <label for="city">City</label>
          <input id="city" value="Testville" />
        </div>
        <div>
          <label for="state">State</label>
          <input id="state" value="CA" />
        </div>
        <div>
          <label for="country">Country</label>
          <input id="country" value="US" />
        </div>
        <div>
          <label for="email">Email</label>
          <input id="email" value="tokenization-test@example.com" />
        </div>
      </div>

      <div class="row" style="margin-top: 12px;">
        <button id="btn-create-pm" type="button">Create payment method</button>
      </div>

      <label for="api-result" style="margin-top:12px;">API response</label>
      <textarea id="api-result" rows="6" readonly placeholder="response will appear here"></textarea>
    </div>

    <div class="card">
      <h2>3) Example API calls (copy/paste)</h2>
      <p class="muted">Replace placeholders (<code>PRICE_ID</code>, <code>PAYMENT_METHOD_ID</code>, etc.). Use <code>X-E2E-Run-ID</code> + <code>X-Idempotency-Key</code> for repeatable runs.</p>
      <pre style="white-space: pre-wrap; background: #fafafa; padding: 12px; border-radius: 10px; border: 1px solid #eee;"><code># Create a checkout session (Mobius subscription)
curl -fsS "https://YOUR_BILLING_HOST/v1/checkout" \
  -H "Authorization: Bearer YOUR_JWT" \
  -H "Content-Type: application/json" \
  -H "X-E2E-Run-ID: YOUR_E2E_RUN_ID" \
  -H "X-Idempotency-Key: e2e_YOUR_E2E_RUN_ID_checkout" \
  --data '{
    "price_id": "price_PRICE_UUID",
    "mode": "subscription",
    "metadata": {"e2e_run_id":"YOUR_E2E_RUN_ID"},
    "payment": {
      "processor": "mobius",
      "payment_method_id": "pm_PAYMENT_METHOD_UUID"
    }
  }'

# Poll status
curl -fsS "https://YOUR_BILLING_HOST/v1/checkout/checkout_session_UUID" \
  -H "Authorization: Bearer YOUR_JWT"</code></pre>
    </div>

    <script src="{{.EffectiveScriptURL}}" data-tokenization-key="{{.TokenizationKey}}"></script>
    <script>
      const provider = {{printf "%q" .Provider}};

      const el = (id) => document.getElementById(id);
      const status = (msg) => { el('status').textContent = msg || ''; };

      function ensureCollect() {
        if (!window.CollectJS) {
          status('CollectJS not loaded yet. If in real mode, check script URL and network.');
          return false;
        }
        return true;
      }

      function configureCollect() {
        if (!ensureCollect()) return;
        try {
          window.CollectJS.configure({
            variant: 'inline',
            fields: {
              ccnumber: { selector: '#ccnumber', placeholder: '0000 0000 0000 0000' },
              ccexp: { selector: '#ccexp', placeholder: '10 / 30' },
              cvv: { selector: '#cvv', placeholder: '123' },
            },
            callback: (resp) => {
              const token = resp && resp.token ? String(resp.token) : '';
              el('token').value = token;
              status(token ? 'Token received.' : 'No token found in Collect.js response.');
            }
          });
          status('Collect.js configured.');
        } catch (e) {
          status('Collect.js configure failed: ' + (e && e.message ? e.message : String(e)));
        }
      }

      async function createPaymentMethod() {
        const jwt = (el('jwt').value || '').trim();
        const e2eRunID = (el('e2e_run_id').value || '').trim();
        const token = (el('token').value || '').trim();
        if (!jwt) { el('api-result').value = 'Missing JWT'; return; }
        if (!token) { el('api-result').value = 'Missing payment_token'; return; }

        const body = {
          payment_token: token,
          first_name: el('first_name').value,
          last_name: el('last_name').value,
          address1: el('address1').value,
          city: el('city').value,
          state: el('state').value,
          zip: el('zip').value,
          country: el('country').value,
          email: el('email').value,
          provider: provider,
        };

        try {
          const headers = {
            'Content-Type': 'application/json',
            'Authorization': 'Bearer ' + jwt,
          };
          if (e2eRunID) headers['X-E2E-Run-ID'] = e2eRunID;
          const res = await fetch('/v1/me/payment-methods', {
            method: 'POST',
            headers,
            body: JSON.stringify(body),
          });
          const text = await res.text();
          el('api-result').value = 'HTTP ' + res.status + '\\n' + text;
        } catch (e) {
          el('api-result').value = String(e);
        }
      }

      el('btn-config').addEventListener('click', configureCollect);
      el('btn-token').addEventListener('click', () => {
        if (!ensureCollect()) return;
        try {
          window.CollectJS.startPaymentRequest();
          status('Requested token...');
        } catch (e) {
          status('Collect.js startPaymentRequest failed: ' + (e && e.message ? e.message : String(e)));
        }
      });
      el('btn-create-pm').addEventListener('click', createPaymentMethod);
      el('btn-copy-token').addEventListener('click', async () => {
        const token = (el('token').value || '').trim();
        if (!token) { status('No token to copy.'); return; }
        try {
          if (navigator.clipboard && navigator.clipboard.writeText) {
            await navigator.clipboard.writeText(token);
            status('Token copied to clipboard.');
            return;
          }
        } catch (e) {
        }
        el('token').focus();
        el('token').select();
        status('Token selected (press Ctrl/Cmd+C).');
      });

      setTimeout(() => {
        if (window.CollectJS) configureCollect();
      }, 50);
    </script>
  </body>
</html>
`))

func (s *Server) debugMobiusTokenization(c *gin.Context) {
	mode := strings.TrimSpace(strings.ToLower(c.Query("mode")))
	if mode != "stub" {
		mode = "real"
	}

	provider := strings.TrimSpace(strings.ToLower(c.Query("provider")))
	if provider == "" {
		provider = "mobius"
	}

	proc := s.cfg.GetProcessor(provider)
	tokenizationKey := ""
	tokenizationURL := ""
	if proc != nil {
		tokenizationKey = strings.TrimSpace(proc.TokenizationKey)
		tokenizationURL = strings.TrimSpace(proc.TokenizationURL)
	}

	effectiveScriptURL := tokenizationURL
	if mode == "stub" || effectiveScriptURL == "" {
		effectiveScriptURL = "/debug/mobius/collect-stub.js"
	}

	hint := "(unset)"
	if tokenizationKey != "" {
		if len(tokenizationKey) <= 6 {
			hint = tokenizationKey
		} else {
			hint = tokenizationKey[:3] + "..." + tokenizationKey[len(tokenizationKey)-3:]
		}
	}

	data := debugMobiusTokenizationPageData{
		Provider:            provider,
		Mode:                mode,
		TokenizationKey:     tokenizationKey,
		TokenizationURL:     tokenizationURL,
		EffectiveScriptURL:  effectiveScriptURL,
		EffectiveKeyHint:    hint,
		HasTokenizationKey:  tokenizationKey != "",
		HasTokenizationURL:  tokenizationURL != "",
		IsConfiguredForReal: tokenizationKey != "" && tokenizationURL != "",
	}

	var buf bytes.Buffer
	if err := debugMobiusTokenizationTemplate.Execute(&buf, data); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to render debug page"})
		return
	}

	c.Data(http.StatusOK, "text/html; charset=utf-8", buf.Bytes())
}

func (s *Server) debugMobiusCollectStubJS(c *gin.Context) {
	js := `(function(){
if (window.CollectJS) return;
var cfg = null;
window.CollectJS = {
  configure: function(c){ cfg = c; },
  startPaymentRequest: function(){
    if (!cfg || typeof cfg.callback !== 'function') return;
    var now = Date.now();
    var token = 'tok_stub_' + now;
    setTimeout(function(){
      cfg.callback({
        token: token,
        tokenType: 'card',
        card: { number: '1111', exp: '1030', type: 'visa', hash: 'stub' }
      });
    }, 10);
  }
};
})();`
	c.Data(http.StatusOK, "application/javascript; charset=utf-8", []byte(js))
}
