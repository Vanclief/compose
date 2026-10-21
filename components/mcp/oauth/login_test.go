package oauth

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"testing"

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

func TestLoginFailuresVoidRequest(t *testing.T) {
	env := newEnv(t, nil, nil)
	_, challenge := pkce("a")
	id := env.startLogin(t, authorizeParams("public", challenge))

	for i := 1; i < maxLoginAttempts; i++ {
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
	for i := 2; i < maxLoginAttempts; i++ {
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

	for i := 1; i < maxLoginAttempts; i++ {
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
	for i := 2; i < maxLoginAttempts; i++ {
		res = env.login(t, id, "ana@acme.com", "")
		require.Equal(t, http.StatusOK, res.status)
		assert.Contains(t, res.body, "Something went wrong. Try again.")
		assert.NotContains(t, res.body, "sms gateway")
	}
	res = env.login(t, id, "ana@acme.com", "")
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "Login link expired")
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
