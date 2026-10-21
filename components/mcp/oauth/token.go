package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/vanclief/ez"
)

// oauthError is the RFC 6749 error body.
type oauthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

// writeOAuthError answers with an RFC 6749 error body.
func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, oauthError{Error: code, Description: description})
}

// tokenResponse is the RFC 6749 token response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope,omitempty"`
}

// sameResource reports whether a resource parameter names Resource: scheme
// and host compare without case, the path exactly after trimming one
// trailing slash. An absent parameter matches.
func (s *Server) sameResource(resource string) bool {
	if resource == "" {
		return true
	}
	u, err := url.Parse(resource)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	want, err := url.Parse(s.cfg.Resource)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, want.Scheme) && strings.EqualFold(u.Host, want.Host) && strings.TrimSuffix(u.Path, "/") == want.Path
}

// serveAuthorize starts the code flow. Errors before the client and its
// redirect URI are known are shown to the user; later errors go back to the
// redirect URI of clients from Config.Clients and are shown to the user for
// the others: registration is open, so redirecting their errors would make
// this endpoint an open redirector.
func (s *Server) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	client, err := s.lookupClient(r.Context(), query.Get("client_id"))
	if err != nil {
		s.renderAuthorizeError(w, r, err)
		return
	}
	redirectURI := query.Get("redirect_uri")
	if redirectURI == "" && len(client.RedirectURIs) == 1 {
		redirectURI = client.RedirectURIs[0]
	}
	if !slices.Contains(client.RedirectURIs, redirectURI) {
		s.renderMessage(w, http.StatusBadRequest, "Invalid request", "The redirect URI is not registered for this application.")
		return
	}
	// Checked here as well as at registration, so a client stored before
	// AllowedRedirectHosts was configured is held to it too.
	if !client.configured && !s.allowedRedirectHosts([]string{redirectURI}) {
		s.renderMessage(w, http.StatusBadRequest, "Invalid request", "The redirect URI is on a host this server does not accept.")
		return
	}

	state := query.Get("state")
	fail := func(code, description string) {
		if client.configured {
			http.Redirect(w, r, withParams(redirectURI, "error", code, "error_description", description, "state", state), http.StatusFound)
			return
		}
		status := http.StatusBadRequest
		if code == "server_error" {
			status = http.StatusInternalServerError
		}
		s.renderMessage(w, status, "Invalid request", description)
	}
	if query.Get("response_type") != "code" {
		fail("unsupported_response_type", "response_type must be code")
		return
	}
	if query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with code_challenge_method S256 is required")
		return
	}
	if !s.sameResource(query.Get("resource")) {
		fail("invalid_target", "resource must be "+s.cfg.Resource)
		return
	}

	// A dynamically registered client is stored again with a fresh expiry,
	// so active clients never expire while dormant ones do.
	if !client.configured && !isDocumentURL(client.ID) {
		err = s.putRecord(r.Context(), KindClient, client.ID, client, time.Now().Add(registeredClientTTL))
		if err != nil {
			s.report(r.Context(), err)
			fail("server_error", "the client could not be stored")
			return
		}
	}

	id := randomID()
	request := authRequest{
		ClientID:      client.ID,
		ClientName:    client.Name,
		RedirectURI:   redirectURI,
		State:         state,
		Scope:         query.Get("scope"),
		CodeChallenge: query.Get("code_challenge"),
		ExpiresAt:     time.Now().Add(requestTTL),
	}
	err = s.putRecord(r.Context(), KindRequest, id, request, request.ExpiresAt)
	if err != nil {
		s.report(r.Context(), err)
		fail("server_error", "the request could not be stored")
		return
	}

	login := s.cfg.Issuer + "/oauth/login"
	if s.cfg.LoginURL != "" {
		login = s.cfg.LoginURL
	}
	http.Redirect(w, r, withParams(login, "request", id), http.StatusFound)
}

// renderAuthorizeError shows why the client could not be resolved.
func (s *Server) renderAuthorizeError(w http.ResponseWriter, r *http.Request, err error) {
	switch ez.ErrorCode(err) {
	case ez.ENOTFOUND:
		s.renderMessage(w, http.StatusBadRequest, "Unknown application", "The application that sent you here is not registered.")
	case ez.EINVALID:
		s.renderMessage(w, http.StatusBadRequest, "Invalid application", ez.ErrorMessage(err))
	case ez.EUNAVAILABLE:
		s.renderMessage(w, http.StatusServiceUnavailable, "Application unavailable", "The application that sent you here could not be reached. Try again in a moment.")
	default:
		s.renderFailure(w, r, err)
	}
}

// withParams adds query parameters to raw, skipping empty values. pairs
// alternates names and values.
func withParams(raw string, pairs ...string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := u.Query()
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			query.Set(pairs[i], pairs[i+1])
		}
	}
	u.RawQuery = query.Encode()
	return u.String()
}

// Pending returns the client and redirect host of a pending request, for a
// login page set with LoginURL to show before the user approves. An unknown
// or expired request yields ez.ENOTFOUND.
func (s *Server) Pending(ctx context.Context, requestID string) (Pending, error) {
	var request authRequest
	err := s.getRecord(ctx, KindRequest, requestID, false, &request)
	if err != nil {
		return Pending{}, ez.Wrap(err)
	}
	return Pending{ClientID: request.ClientID, ClientName: request.ClientName, RedirectHost: redirectHost(request.RedirectURI)}, nil
}

// Approve redeems the pending request and returns the URL the browser must
// be sent to: the client redirect URI carrying the authorization code and
// state. An unknown or expired request yields ez.ENOTFOUND.
func (s *Server) Approve(ctx context.Context, requestID string, grant Grant) (string, error) {
	var request authRequest
	err := s.getRecord(ctx, KindRequest, requestID, true, &request)
	if err != nil {
		return "", ez.Wrap(err)
	}
	redirect, err := s.approve(ctx, request, grant)
	if err != nil {
		return "", ez.Wrap(err)
	}
	return redirect, nil
}

// approve issues the code of a request already taken from the Store.
func (s *Server) approve(ctx context.Context, request authRequest, grant Grant) (string, error) {
	code := randomID()
	record := codeRecord{
		ClientID:      request.ClientID,
		RedirectURI:   request.RedirectURI,
		Scope:         request.Scope,
		CodeChallenge: request.CodeChallenge,
		Grant:         grant,
	}
	err := s.putRecord(ctx, KindCode, code, record, time.Now().Add(codeTTL))
	if err != nil {
		return "", ez.Wrap(err)
	}
	return withParams(request.RedirectURI, "code", code, "state", request.State), nil
}

// serveToken exchanges a code or a refresh token for a new token pair.
func (s *Server) serveToken(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body must be form encoded")
		return
	}
	grantType := r.PostForm.Get("grant_type")
	if grantType == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "grant_type is required")
		return
	}
	if grantType != "authorization_code" && grantType != "refresh_token" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
		return
	}
	if !s.sameResource(r.PostForm.Get("resource")) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource must be "+s.cfg.Resource)
		return
	}
	client, ok := s.authenticateClient(w, r)
	if !ok {
		return
	}
	if grantType == "authorization_code" {
		s.exchangeCode(w, r, client)
		return
	}
	s.exchangeRefresh(w, r, client)
}

// authenticateClient identifies the client of a token request. Confidential
// clients authenticate with client_secret_basic or client_secret_post;
// public clients send their client_id. A Client ID Metadata Document is
// never fetched here: its client is public, and the code or refresh token
// already binds the client_id, so an outage at the document host cannot
// fail a token request. On failure it writes the error and returns false.
func (s *Server) authenticateClient(w http.ResponseWriter, r *http.Request) (Client, bool) {
	basicID, basicSecret, basic := r.BasicAuth()
	fail := func() (Client, bool) {
		if basic {
			w.Header().Set("WWW-Authenticate", `Basic realm="oauth"`)
		}
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return Client{}, false
	}

	clientID := r.PostForm.Get("client_id")
	secret := r.PostForm.Get("client_secret")
	if basic {
		// RFC 6749 2.3.1: both parts are form-encoded before Basic encoding.
		id, err := url.QueryUnescape(basicID)
		if err != nil {
			return fail()
		}
		secret, err = url.QueryUnescape(basicSecret)
		if err != nil {
			return fail()
		}
		if clientID != "" && clientID != id {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_id differs from the Basic credentials")
			return Client{}, false
		}
		clientID = id
	}
	if clientID == "" {
		return fail()
	}
	client, err := s.knownClient(r.Context(), clientID)
	if err != nil && ez.ErrorCode(err) != ez.ENOTFOUND {
		s.serverError(r.Context(), w, err, "the client could not be loaded")
		return Client{}, false
	}
	if err != nil {
		if !isDocumentURL(clientID) {
			return fail()
		}
		client = Client{ID: clientID}
	}
	if client.Secret != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(client.Secret)) != 1 {
		return fail()
	}
	return client, true
}

// exchangeCode redeems an authorization code.
func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, client Client) {
	code := r.PostForm.Get("code")
	verifier := r.PostForm.Get("code_verifier")
	if code == "" || verifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code and code_verifier are required")
		return
	}
	var record codeRecord
	err := s.getRecord(r.Context(), KindCode, code, true, &record)
	if err != nil && ez.ErrorCode(err) != ez.ENOTFOUND {
		// Not invalid_grant: the client would drop a grant that may be fine.
		s.serverError(r.Context(), w, err, "the code could not be checked")
		return
	}
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the code is invalid, expired or already used")
		return
	}
	// redirect_uri is compared only when sent: PKCE already binds the code
	// to the client that started the flow.
	redirectURI := r.PostForm.Get("redirect_uri")
	if record.ClientID != client.ID || (r.PostForm.Has("redirect_uri") && redirectURI != record.RedirectURI) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued to another client or redirect URI")
		return
	}
	if !verifyPKCE(verifier, record.CodeChallenge) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the code challenge")
		return
	}
	err = s.issueTokens(r.Context(), w, client.ID, record.Scope, record.Grant, time.Now().Add(s.cfg.RefreshTokenTTL))
	if err != nil {
		s.serverError(r.Context(), w, err, "the token could not be stored")
	}
}

// verifyPKCE checks the S256 code challenge.
func verifyPKCE(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// refreshTimeout bounds the Store calls of a refresh that run detached from
// the request: taking the refresh token and putting it back.
const refreshTimeout = 10 * time.Second

// exchangeRefresh rotates a refresh token. The new refresh token keeps the
// expiry of the first one, so RefreshTokenTTL bounds the whole connection.
//
// The take, the restore and the reporting run on a context detached from the
// request, so a client that disconnects mid-refresh cannot leave the token
// taken and never put back, which would answer its retry invalid_grant.
// Issuing stays on the request context: a client that is gone gets no new
// tokens it cannot receive, and the restore keeps the one it holds.
func (s *Server) exchangeRefresh(w http.ResponseWriter, r *http.Request, client Client) {
	token := r.PostForm.Get("refresh_token")
	if token == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), refreshTimeout)
	defer cancel()
	var record tokenRecord
	err := s.getRecord(ctx, KindRefresh, token, true, &record)
	if err != nil && ez.ErrorCode(err) != ez.ENOTFOUND {
		// Not invalid_grant: the client would drop a token that may be fine.
		s.serverError(ctx, w, err, "the refresh token could not be checked")
		return
	}
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid, expired or already used")
		return
	}
	if record.ClientID != client.ID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was issued to another client")
		return
	}
	err = s.issueTokens(r.Context(), w, client.ID, record.Scope, record.Grant, record.ExpiresAt)
	if err != nil {
		s.restoreRefresh(ctx, token, record)
		s.serverError(ctx, w, err, "the token could not be stored")
	}
}

// restoreRefresh puts a taken refresh token back after issuing failed, so
// the client can retry instead of losing the connection.
func (s *Server) restoreRefresh(ctx context.Context, token string, record tokenRecord) {
	err := s.putRecord(ctx, KindRefresh, token, record, record.ExpiresAt)
	if err != nil {
		s.report(ctx, err)
	}
}

// issueTokens stores a new access and refresh token and writes the token
// response. On error nothing has been written.
func (s *Server) issueTokens(ctx context.Context, w http.ResponseWriter, clientID, scope string, grant Grant, refreshExpiresAt time.Time) error {
	access := randomID()
	refresh := randomID()
	accessExpiresAt := time.Now().Add(s.cfg.AccessTokenTTL)
	err := s.putRecord(ctx, KindAccess, access, tokenRecord{ClientID: clientID, Scope: scope, Grant: grant, ExpiresAt: accessExpiresAt}, accessExpiresAt)
	if err != nil {
		return ez.Wrap(err)
	}
	err = s.putRecord(ctx, KindRefresh, refresh, tokenRecord{ClientID: clientID, Scope: scope, Grant: grant, ExpiresAt: refreshExpiresAt}, refreshExpiresAt)
	if err != nil {
		return ez.Wrap(err)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    int64(s.cfg.AccessTokenTTL / time.Second),
		RefreshToken: refresh,
		Scope:        scope,
	})
	return nil
}

// accessGrant returns the grant of a live access token.
func (s *Server) accessGrant(ctx context.Context, token string) (tokenRecord, error) {
	var record tokenRecord
	err := s.getRecord(ctx, KindAccess, token, false, &record)
	if err != nil {
		return tokenRecord{}, ez.Wrap(err)
	}
	return record, nil
}

// Protect wraps the MCP handler: requests without a valid bearer token get
// 401 with the WWW-Authenticate challenge pointing at the protected resource
// metadata.
func (s *Server) Protect(next http.Handler) http.Handler {
	require := auth.RequireBearerToken(s.verifyToken, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL:    s.resourceMetadataURL(),
		AllowMissingExpiration: true,
	})
	return require(next)
}

// verifyToken is the auth.TokenVerifier of Protect. Unknown tokens wrap
// auth.ErrInvalidToken so the middleware answers 401; Store failures answer
// 500. The middleware writes the error text as the response body.
func (s *Server) verifyToken(ctx context.Context, token string, r *http.Request) (*auth.TokenInfo, error) {
	record, err := s.accessGrant(ctx, token)
	if err == nil {
		return &auth.TokenInfo{Expiration: record.ExpiresAt, UserID: record.Grant.Subject}, nil
	}
	if ez.ErrorCode(err) != ez.ENOTFOUND {
		// The middleware writes the error text to the response, so the
		// Store error goes to OnError and stays out of it.
		s.report(ctx, err)
		return nil, ez.New(ez.EINTERNAL, "oauth: the access token could not be checked", nil)
	}
	// Not ez errors: their text reaches the client, and ez adds the
	// operation name to it.
	if s.cfg.Fallback == nil {
		return nil, fmt.Errorf("unknown or expired access token: %w", auth.ErrInvalidToken)
	}
	_, err = s.cfg.Fallback(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("the bearer token is not accepted: %w", auth.ErrInvalidToken)
	}
	return &auth.TokenInfo{}, nil
}

// AuthFunc maps a bearer token to the REST headers of its grant, for
// mcp.Config.Auth. Unknown tokens go to Fallback when it is set, else fail
// with ez.ENOTAUTHENTICATED. An empty token yields no headers.
func (s *Server) AuthFunc() func(ctx context.Context, token string) (http.Header, error) {
	return func(ctx context.Context, token string) (http.Header, error) {
		if token == "" {
			return http.Header{}, nil
		}
		record, err := s.accessGrant(ctx, token)
		if err == nil {
			return record.Grant.Headers.Clone(), nil
		}
		if ez.ErrorCode(err) != ez.ENOTFOUND {
			s.report(ctx, err)
			return nil, ez.Wrap(err)
		}
		if s.cfg.Fallback == nil {
			return nil, ez.New(ez.ENOTAUTHENTICATED, "oauth: unknown or expired access token", nil)
		}
		header, err := s.cfg.Fallback(ctx, token)
		if err != nil {
			return nil, ez.Wrap(err)
		}
		return header, nil
	}
}
