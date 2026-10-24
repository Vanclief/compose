package oauth

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func TestLoginPasswordFlow(t *testing.T) {
	env := newEnv(t, nil, nil)
	_, challenge := pkce("a")
	id := env.startLogin(t, authorizeParams("public", challenge))

	page := env.get(t, "/oauth/login?request="+id)
	require.Equal(t, http.StatusOK, page.status)
	assert.Equal(t, "no-store", page.header.Get("Cache-Control"))
	assert.Equal(t, "DENY", page.header.Get("X-Frame-Options"))
	assert.Equal(t, "text/html; charset=utf-8", page.header.Get("Content-Type"))
	assert.Contains(t, page.body, "Sign in to Acme")
	assert.Contains(t, page.body, `Public App</span> at <span class="host">client.example</span> wants to access your Acme account.`)
	assert.Contains(t, page.body, `<span class="host">client.example</span>`)
	assert.Contains(t, page.body, `name="identifier"`)
	assert.Contains(t, page.body, `name="credential" type="password"`)
	assert.Contains(t, page.body, `<label for="identifier">Email</label>`)
	assert.Contains(t, page.body, `<label for="credential">Password</label>`)
	assert.Contains(t, page.body, `value="`+id+`"`)

	res := env.login(t, id, "ana@acme.com", "wrong")
	assert.Equal(t, http.StatusOK, res.status)
	assert.Contains(t, res.body, "Wrong email or password.")
	assert.Contains(t, res.body, `value="ana@acme.com"`, "the identifier is kept")

	res = env.login(t, id, "", "pw")
	assert.Contains(t, res.body, "Enter your email.")
	res = env.login(t, id, "ana@acme.com", "")
	assert.Contains(t, res.body, "Enter your password.")

	res = env.login(t, id, "ana@acme.com", "pw")
	assert.Equal(t, http.StatusSeeOther, res.status)
	assert.Equal(t, "no-store", res.header.Get("Cache-Control"))
	codeFrom(t, res)

	// The request is redeemed.
	res = env.login(t, id, "ana@acme.com", "pw")
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "This login link has expired. Start again from the application.")
}

// TestLoginVerifyScope: a Grant.Scope returned by Login.Verify is what the
// tokens carry, not the scope the client requested.
func TestLoginVerifyScope(t *testing.T) {
	env := newEnv(t, func(c *Config) {
		c.Login.Verify = func(ctx context.Context, identifier, credential string) (Grant, error) {
			return Grant{Subject: "user-1", Scope: "read"}, nil
		}
	}, nil)
	verifier, challenge := pkce("v")
	params := authorizeParams("public", challenge)
	params.Set("scope", "write")
	id := env.startLogin(t, params)

	code := codeFrom(t, env.login(t, id, "ana@acme.com", "pw"))
	res := env.exchange(t, code, verifier)
	require.Equal(t, http.StatusOK, res.status, res.body)
	body := res.json(t)
	assert.Equal(t, "read", body["scope"])

	grant, err := env.s.Verify(context.Background(), body["access_token"].(string))
	require.NoError(t, err)
	assert.Equal(t, Grant{Subject: "user-1", Scope: "read"}, grant)
}

func TestLoginChallengeFlow(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	env := newEnv(t, func(c *Config) {
		c.Login.IdentifierLabel = "Email or phone"
		c.Login.Challenge = func(ctx context.Context, identifier string) error {
			if identifier == "nobody@acme.com" {
				return ez.New(ez.ENOTFOUND, "No account uses that email.", nil)
			}
			mu.Lock()
			defer mu.Unlock()
			sent = append(sent, identifier)
			return nil
		}
	}, nil)
	_, challenge := pkce("a")
	id := env.startLogin(t, authorizeParams("public", challenge))

	page := env.get(t, "/oauth/login?request="+id)
	assert.Contains(t, page.body, `<label for="identifier">Email or phone</label>`)
	assert.NotContains(t, page.body, `name="credential"`)
	assert.Contains(t, page.body, "Continue")

	res := env.login(t, id, "nobody@acme.com", "")
	assert.Contains(t, res.body, "No account uses that email.")
	assert.NotContains(t, res.body, `name="credential"`)

	res = env.login(t, id, " ana@acme.com ", "")
	assert.Equal(t, http.StatusOK, res.status)
	assert.Contains(t, res.body, `<div class="identifier">ana@acme.com</div>`)
	assert.NotContains(t, res.body, `name="identifier"`)
	assert.Contains(t, res.body, `name="credential" type="text" inputmode="numeric" autocomplete="one-time-code"`)
	assert.Contains(t, res.body, `<label for="credential">Code</label>`)
	mu.Lock()
	assert.Equal(t, []string{"ana@acme.com"}, sent)
	mu.Unlock()

	// Reloading the page stays on the credential step.
	page = env.get(t, "/oauth/login?request="+id)
	assert.Contains(t, page.body, `<div class="identifier">ana@acme.com</div>`)

	// The stored identifier wins over a submitted one.
	res = env.login(t, id, "eve@acme.com", "wrong")
	assert.Contains(t, res.body, "Wrong email or password.")
	res = env.login(t, id, "eve@acme.com", "pw")
	codeFrom(t, res)
	mu.Lock()
	assert.Len(t, sent, 1)
	mu.Unlock()
}

// TestLoginResubmitKeepsExpiry: resubmitting the form, even with empty
// fields that count no attempt, never extends the pending request.
func TestLoginResubmitKeepsExpiry(t *testing.T) {
	store := NewMemoryStore().(*memoryStore)
	env := newEnv(t, func(c *Config) {
		c.Store = store
	}, nil)
	_, challenge := pkce("a")
	id := env.startLogin(t, authorizeParams("public", challenge))
	expiry := func() time.Time {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.records[memoryKey(KindRequest, id)].expiresAt
	}
	created := expiry()
	assert.WithinDuration(t, time.Now().Add(env.s.cfg.RequestTTL), created, time.Minute)

	for i := 0; i < 2*env.s.cfg.MaxLoginAttempts; i++ {
		res := env.login(t, id, "", "")
		require.Equal(t, http.StatusOK, res.status)
		assert.Contains(t, res.body, "Enter your email.")
		res = env.login(t, id, "ana@acme.com", "")
		require.Equal(t, http.StatusOK, res.status)
		assert.Contains(t, res.body, "Enter your password.")
		assert.True(t, created.Equal(expiry()), "resubmit %d moved the expiry to %s", i, expiry())
	}
	res := env.login(t, id, "ana@acme.com", "wrong")
	assert.Contains(t, res.body, "Wrong email or password.")
	assert.True(t, created.Equal(expiry()))
	codeFrom(t, env.login(t, id, "ana@acme.com", "pw"))
}

func TestLoginFailuresVoidRequest(t *testing.T) {
	env := newEnv(t, nil, nil)
	_, challenge := pkce("a")
	id := env.startLogin(t, authorizeParams("public", challenge))

	for i := 1; i < env.s.cfg.MaxLoginAttempts; i++ {
		res := env.login(t, id, "ana@acme.com", "wrong")
		require.Equal(t, http.StatusOK, res.status)
		assert.Contains(t, res.body, "Wrong email or password.")
	}
	res := env.login(t, id, "ana@acme.com", "wrong")
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "Login link expired")

	res = env.login(t, id, "ana@acme.com", "pw")
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "Login link expired")
	assert.Equal(t, http.StatusBadRequest, env.get(t, "/oauth/login?request="+id).status)
}

func TestLoginServerError(t *testing.T) {
	var reported []error
	env := newEnv(t, func(c *Config) {
		c.OnError = func(ctx context.Context, err error) {
			reported = append(reported, err)
		}
	}, nil)
	_, challenge := pkce("a")
	id := env.startLogin(t, authorizeParams("public", challenge))

	res := env.login(t, id, "boom", "pw")
	assert.Equal(t, http.StatusOK, res.status)
	assert.Contains(t, res.body, "Something went wrong. Try again.")
	assert.NotContains(t, res.body, "database is down")
	require.Len(t, reported, 1, "the hidden Verify error reaches OnError")
	assert.Contains(t, reported[0].Error(), "database is down")

	// Server errors count as attempts too.
	for i := 2; i < env.s.cfg.MaxLoginAttempts; i++ {
		res = env.login(t, id, "boom", "pw")
		assert.Contains(t, res.body, "Something went wrong. Try again.")
	}
	res = env.login(t, id, "boom", "pw")
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "Login link expired")
}

func TestLoginVerifyErrorsCount(t *testing.T) {
	env := newEnv(t, func(c *Config) {
		c.Login.Verify = func(ctx context.Context, identifier, credential string) (Grant, error) {
			return Grant{}, ez.New(ez.ENOTAUTHORIZED, "account suspended for ana@acme.com", nil)
		}
	}, nil)
	_, challenge := pkce("a")
	id := env.startLogin(t, authorizeParams("public", challenge))

	for i := 1; i < env.s.cfg.MaxLoginAttempts; i++ {
		res := env.login(t, id, "ana@acme.com", "pw")
		require.Equal(t, http.StatusOK, res.status)
		assert.Contains(t, res.body, "Something went wrong. Try again.")
		assert.NotContains(t, res.body, "suspended")
	}
	res := env.login(t, id, "ana@acme.com", "pw")
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "Login link expired")
}

func TestLoginChallengeErrorsCount(t *testing.T) {
	env := newEnv(t, func(c *Config) {
		c.Login.Challenge = func(ctx context.Context, identifier string) error {
			if identifier == "busy@acme.com" {
				return ez.New(ez.ERESOURCEEXHAUSTED, "Too many codes sent. Wait a minute.", nil)
			}
			return ez.New(ez.EINTERNAL, "sms gateway down", nil)
		}
	}, nil)
	_, challenge := pkce("a")
	id := env.startLogin(t, authorizeParams("public", challenge))

	res := env.login(t, id, "busy@acme.com", "")
	assert.Contains(t, res.body, "Too many codes sent. Wait a minute.")
	for i := 2; i < env.s.cfg.MaxLoginAttempts; i++ {
		res = env.login(t, id, "ana@acme.com", "")
		require.Equal(t, http.StatusOK, res.status)
		assert.Contains(t, res.body, "Something went wrong. Try again.")
		assert.NotContains(t, res.body, "sms gateway")
	}
	res = env.login(t, id, "ana@acme.com", "")
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "Login link expired")
}

func TestLoginGrantWithoutSubject(t *testing.T) {
	var reported []error
	env := newEnv(t, func(c *Config) {
		c.Login.Verify = func(ctx context.Context, identifier, credential string) (Grant, error) {
			return Grant{}, nil
		}
		c.OnError = func(ctx context.Context, err error) {
			reported = append(reported, err)
		}
	}, nil)
	_, challenge := pkce("a")
	id := env.startLogin(t, authorizeParams("public", challenge))

	res := env.login(t, id, "ana@acme.com", "pw")
	assert.Equal(t, http.StatusInternalServerError, res.status)
	assert.Empty(t, res.header.Get("Location"), "no code is issued")
	require.Len(t, reported, 1)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(reported[0]))
}

func TestLoginUnknownRequest(t *testing.T) {
	env := newEnv(t, nil, nil)
	for _, res := range []response{
		env.get(t, "/oauth/login"),
		env.get(t, "/oauth/login?request=unknown"),
		env.postForm(t, "/oauth/login", url.Values{"request": {"unknown"}, "identifier": {"ana@acme.com"}, "credential": {"pw"}}),
	} {
		assert.Equal(t, http.StatusBadRequest, res.status)
		assert.Contains(t, res.body, "This login link has expired. Start again from the application.")
		assert.NotContains(t, res.body, "<form")
		assert.Equal(t, "no-store", res.header.Get("Cache-Control"))
	}
}

func TestLoginUnnamedClient(t *testing.T) {
	env := newEnv(t, func(c *Config) {
		c.Login.Title = ""
	}, nil)
	res := env.postJSON(t, "/oauth/register", `{"redirect_uris": ["`+testRedirect+`"]}`)
	clientID := res.json(t)["client_id"].(string)
	_, challenge := pkce("a")
	id := env.startLogin(t, authorizeParams(clientID, challenge))
	page := env.get(t, "/oauth/login?request="+id)
	assert.Contains(t, page.body, "<title>Sign in</title>")
	assert.Contains(t, page.body, `An application</span> at <span class="host">client.example</span> wants to access your account.`)
}

func TestLoginMaxAttemptsConfigured(t *testing.T) {
	env := newEnv(t, func(c *Config) {
		c.MaxLoginAttempts = 2
	}, nil)
	_, challenge := pkce("a")
	id := env.startLogin(t, authorizeParams("public", challenge))

	res := env.login(t, id, "ana@acme.com", "wrong")
	require.Equal(t, http.StatusOK, res.status)
	assert.Contains(t, res.body, "Wrong email or password.")
	res = env.login(t, id, "ana@acme.com", "wrong")
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "Login link expired")
	res = env.login(t, id, "ana@acme.com", "pw")
	assert.Equal(t, http.StatusBadRequest, res.status, "the voided request cannot be approved")
}
