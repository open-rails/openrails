(function(){
  var cfg = {
    enabled: __OPENRAILS_CAPTCHA_ENABLED__,
    provider: __OPENRAILS_CAPTCHA_PROVIDER__,
    siteKey: __OPENRAILS_CAPTCHA_SITE_KEY__,
    scriptURL: __OPENRAILS_CAPTCHA_SCRIPT_URL__
  };
  var providerPromise = null;
  var widgetId = null;

  function providerReady() {
    if (!cfg.enabled) return true;
    if (cfg.provider === "turnstile") return !!(window.turnstile && window.turnstile.render);
    if (cfg.provider === "hcaptcha") return !!(window.hcaptcha && window.hcaptcha.render);
    if (cfg.provider === "recaptcha") return !!(window.grecaptcha && window.grecaptcha.render);
    return false;
  }

  function loadProvider() {
    if (!cfg.enabled) return Promise.resolve();
    if (providerReady()) return Promise.resolve();
    if (providerPromise) return providerPromise;
    providerPromise = new Promise(function(resolve, reject) {
      var existing = document.querySelector('script[data-openrails-captcha-provider="' + cfg.provider + '"]');
      if (existing) {
        existing.addEventListener('load', resolve, { once: true });
        existing.addEventListener('error', function(){ reject(new Error('captcha script failed to load')); }, { once: true });
        return;
      }
      var script = document.createElement('script');
      script.src = cfg.scriptURL;
      script.async = true;
      script.defer = true;
      script.dataset.openrailsCaptchaProvider = cfg.provider;
      script.onload = resolve;
      script.onerror = function(){ reject(new Error('captcha script failed to load')); };
      document.head.appendChild(script);
    });
    return providerPromise;
  }

  function resolveElement(container) {
    var el = typeof container === 'string' ? document.querySelector(container) : container;
    if (!el) throw new Error('captcha container not found');
    return el;
  }

  function renderProvider(el, resolve, reject) {
    el.innerHTML = '';
    var options = {
      sitekey: cfg.siteKey,
      callback: function(token){ resolve(token || ''); },
      'error-callback': function(){ reject(new Error('captcha challenge failed')); },
      'expired-callback': function(){ reset(); }
    };
    if (cfg.provider === 'turnstile') {
      if (!window.turnstile || !window.turnstile.render) return reject(new Error('turnstile is unavailable'));
      widgetId = window.turnstile.render(el, options);
      return;
    }
    if (cfg.provider === 'hcaptcha') {
      if (!window.hcaptcha || !window.hcaptcha.render) return reject(new Error('hcaptcha is unavailable'));
      widgetId = window.hcaptcha.render(el, options);
      return;
    }
    if (cfg.provider === 'recaptcha') {
      if (!window.grecaptcha || !window.grecaptcha.render) return reject(new Error('recaptcha is unavailable'));
      var doRender = function(){ widgetId = window.grecaptcha.render(el, options); };
      if (window.grecaptcha.ready) window.grecaptcha.ready(doRender); else doRender();
      return;
    }
    reject(new Error('unsupported captcha provider'));
  }

  function solve(container) {
    if (!cfg.enabled) return Promise.resolve('');
    return loadProvider().then(function() {
      return new Promise(function(resolve, reject) {
        var el;
        try { el = resolveElement(container); } catch (err) { reject(err); return; }
        renderProvider(el, resolve, reject);
      });
    });
  }

  function reset() {
    try {
      if (cfg.provider === 'turnstile' && window.turnstile && window.turnstile.reset) window.turnstile.reset(widgetId);
      if (cfg.provider === 'hcaptcha' && window.hcaptcha && window.hcaptcha.reset) window.hcaptcha.reset(widgetId);
      if (cfg.provider === 'recaptcha' && window.grecaptcha && window.grecaptcha.reset) window.grecaptcha.reset(widgetId);
    } catch (_) {}
  }

  window.OpenRailsCaptcha = {
    enabled: cfg.enabled,
    provider: cfg.provider,
    load: loadProvider,
    solve: solve,
    reset: reset,
    isEnabled: function(){ return cfg.enabled; }
  };
})();
