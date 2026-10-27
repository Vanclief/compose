package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/vanclief/compose/components/auth/token"
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
	if !redirectRegistered(client, redirectURI) {
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

	err = s.renewClient(r.Context(), client)
	if err != nil {
		s.report(r.Context(), err)
		fail("server_error", "the client could not be stored")
		return
	}

	id := randomID()
	request := authRequest{
		ClientID:      client.ID,
		ClientName:    client.Name,
		RedirectURI:   redirectURI,
		State:         state,
		Scope:         query.Get("scope"),
		CodeChallenge: query.Get("code_challenge"),
		ExpiresAt:     time.Now().Add(s.cfg.RequestTTL),
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

// Pending returns the client, redirect host and requested scope of a pending
// request, for a login page set with LoginURL to show before the user
// approves. An unknown or expired request yields ez.ENOTFOUND.
func (s *Server) Pending(ctx context.Context, requestID string) (Pending, error) {
	var request authRequest
	err := s.getRecord(ctx, KindRequest, requestID, false, &request)
	if err != nil {
		return Pending{}, ez.Wrap(err)
	}
	pending := Pending{
		ClientID:     request.ClientID,
		ClientName:   request.ClientName,
		RedirectHost: redirectHost(request.RedirectURI),
		Scope:        request.Scope,
	}
	return pending, nil
}

// Approve redeems the pending request and returns the URL the browser must
// be sent to: the client redirect URI carrying the authorization code and
// state. grant.Scope is stored exactly as given, empty included; the scope
// the client requested only echoes in the token response. An unknown or
// expired request yields ez.ENOTFOUND, and a grant without Subject
// ez.EINTERNAL.
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

// Deny ends a pending authorization request with access_denied and returns
// the URL to send the browser to, carrying the client's state. Single use:
// the request is taken from the Store. An unknown or expired request is
// ez.ENOTFOUND.
func (s *Server) Deny(ctx context.Context, requestID string) (string, error) {
	var request authRequest
	err := s.getRecord(ctx, KindRequest, requestID, true, &request)
	if err != nil {
		return "", ez.Wrap(err)
	}
	return withParams(request.RedirectURI, "error", "access_denied", "error_description", "The user denied the request.", "state", request.State), nil
}

// approve issues the code of a request already taken from the Store. The
// grant is stored as the application gave it: the scope the client requested
// is kept beside it for the token response and never copied into
// grant.Scope, so Verify only ever returns a scope the application set. A
// grant without Subject is refused: its tokens would carry no identity.
func (s *Server) approve(ctx context.Context, request authRequest, grant Grant) (string, error) {
	if grant.Subject == "" {
		return "", ez.New(ez.EINTERNAL, "oauth: the grant has no subject", nil)
	}
	code := randomID()
	record := codeRecord{
		ClientID:       request.ClientID,
		RedirectURI:    request.RedirectURI,
		CodeChallenge:  request.CodeChallenge,
		Grant:          grant,
		RequestedScope: request.Scope,
	}
	err := s.putRecord(ctx, KindCode, hashID(code), record, time.Now().Add(s.cfg.CodeTTL))
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

// exchangeCode redeems an authorization code, renews a dynamically
// registered client (see renewClient) and starts a token family at
// generation 1.
func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, client Client) {
	code := r.PostForm.Get("code")
	verifier := r.PostForm.Get("code_verifier")
	if code == "" || verifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code and code_verifier are required")
		return
	}
	var record codeRecord
	err := s.getRecord(r.Context(), KindCode, hashID(code), true, &record)
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
	family := familyRecord{
		ClientID:       client.ID,
		Grant:          record.Grant,
		RequestedScope: record.RequestedScope,
		Generation:     1,
	}
	if s.cfg.RefreshTokenTTL == Never {
		family.Idle = s.cfg.IdleTTL
		family.ExpiresAt = time.Now().Add(s.cfg.IdleTTL)
	} else {
		family.ExpiresAt = time.Now().Add(s.cfg.RefreshTokenTTL)
	}
	familyID := randomID()
	// The client is renewed before the tokens are written, best effort, so
	// the commit stays the last write before the response: its failure is
	// only reported, and a Store that is down fails the writes below anyway.
	err = s.renewClient(r.Context(), client)
	if err != nil {
		s.report(r.Context(), err)
	}
	// The token rows go first and are inert until the family row exists, so
	// storing the family is the commit. A failure leaves only inert rows,
	// and the code is spent: the user authorizes again.
	pair, err := s.putTokens(r.Context(), familyID, family)
	if err != nil {
		s.serverError(r.Context(), w, err, "the token could not be stored")
		return
	}
	family.Refresh = hashID(pair.refresh)
	err = s.putRecord(r.Context(), KindFamily, familyID, family, family.ExpiresAt)
	if err != nil {
		s.serverError(r.Context(), w, err, "the token could not be stored")
		return
	}
	writeTokens(w, pair, family)
}

// verifyPKCE checks the S256 code challenge.
func verifyPKCE(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// exchangeRefresh rotates a refresh token. The family row is the single
// source of truth (see familyRecord): a refresh token rotates only while its
// generation is the family's, and the rotation commits with one Swap of the
// family row to the next generation, the last write before the response. The
// new tokens take the expiry of the family, so RefreshTokenTTL bounds the
// whole connection; a family issued under Never has its expiry moved to
// Idle from now first, so the connection ends Idle after its last refresh.
//
// Every failure before the Swap leaves the presented token as it was, so the
// client's retry rotates it. The new rows already written stay behind: the
// refresh row never rotates, since the family never names it, and the
// access row is refused until a retry commits its generation, then
// accepted, which exposes nothing as it was never sent to a client. A token
// of another generation than the family's was rotated already, or a
// concurrent request presenting it won the Swap: either way it was presented
// twice, and revokeFamily deletes the family. That includes a client that
// lost the response of a committed rotation and retries: the user logs in
// again, which is preferred to a reuse grace period because a rotated token
// stays dead.
//
// The Store calls run on a context detached from the request, under
// RefreshTimeout, so a client that disconnects cannot stop a rotation
// halfway once the Swap has started. A client gone before the Swap gets no
// commit and no response, so its retry with the same token rotates it.
// Before the new tokens are written a dynamically registered client is
// renewed (see renewClient), so a client that keeps refreshing never
// expires; the renewal only reports its failure.
func (s *Server) exchangeRefresh(w http.ResponseWriter, r *http.Request, client Client) {
	refreshToken := r.PostForm.Get("refresh_token")
	if refreshToken == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	// A token of the wrong shape was never issued here, so it is answered
	// without reading the Store.
	if !token.Check(s.cfg.TokenPrefix+"rt", refreshToken) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid, expired or already used")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.cfg.RefreshTimeout)
	defer cancel()
	id := hashID(refreshToken)
	var record tokenRecord
	err := s.getRecord(ctx, KindRefresh, id, false, &record)
	if err != nil && ez.ErrorCode(err) != ez.ENOTFOUND {
		// Not invalid_grant: the client would drop a token that may be fine.
		s.serverError(ctx, w, err, "the refresh token could not be checked")
		return
	}
	if err != nil {
		// Nothing is known about the token: never issued, expired, or
		// rotated longer ago than reuse detection remembers.
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid, expired or already used")
		return
	}
	family, raw, err := s.getFamily(ctx, record.Family)
	if err != nil && ez.ErrorCode(err) != ez.ENOTFOUND {
		s.serverError(ctx, w, err, "the refresh token could not be checked")
		return
	}
	if err != nil {
		// The family expired or was revoked.
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid, expired or already used")
		return
	}
	if family.ClientID != client.ID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was issued to another client")
		return
	}
	// Only the refresh token the family names rotates. Any other row of the
	// family is a rotated token presented again, or one a failed rotation
	// left behind that nobody holds, and both are answered as reuse: the
	// check fails closed.
	if id != family.Refresh {
		s.revokeFamily(ctx, w, record.Family, client.ID)
		return
	}

	next := family
	next.Generation++
	if family.Idle > 0 {
		next.ExpiresAt = time.Now().Add(family.Idle)
	}
	// The client is renewed before the tokens are written, best effort, so
	// the Swap stays the last write before the response: its failure is only
	// reported, and a Store that is down fails the writes below anyway. The
	// presented token stays a dead row of the old generation until the
	// expiry it was issued with, so a replay is detected until then.
	err = s.renewClient(ctx, client)
	if err != nil {
		s.report(ctx, err)
	}
	pair, err := s.putTokens(ctx, record.Family, next)
	if err != nil {
		s.serverError(ctx, w, err, "the token could not be stored")
		return
	}
	next.Refresh = hashID(pair.refresh)
	// A client gone before the Swap, its own timeout fired while the Store
	// was slow for instance, never reads the new tokens and retries with the
	// presented one, which a committed rotation would answer as reuse. So
	// nothing commits and nothing is answered: the rows written so far are
	// never sent, and the retry rotates normally. Once started, the Swap
	// runs to the end.
	if r.Context().Err() != nil {
		return
	}
	err = s.swapFamily(ctx, record.Family, raw, next)
	if err != nil && ez.ErrorCode(err) == ez.ENOTFOUND {
		s.lostRotation(ctx, w, id, record.Family, client.ID)
		return
	}
	if err != nil && !s.committed(r.Context(), record.Family, next.Refresh) {
		s.serverError(ctx, w, err, "the token could not be stored")
		return
	}
	if err != nil {
		// The Swap committed and then failed, on its deadline for instance.
		// Answering server_error would make the client retry with a rotated
		// token and lose the connection, so the rotation is answered as the
		// success it was, and the failure only reported.
		s.report(ctx, err)
	}
	writeTokens(w, pair, next)
}

// revokeFamily answers a refresh token presented again, after or during its
// rotation, by deleting the family row. Every access and refresh token of the
// family points at that row, so all of them are refused from their next
// check, and nothing else has to be found or deleted: their rows expire on
// their own. The reuse is reported with the family and client ids, and
// answered invalid_grant.
//
// When the delete fails the answer is server_error instead: a legitimate
// client answered invalid_grant would log in anew while a thief's tokens live
// on, whereas its retry with the same token repeats the detection.
func (s *Server) revokeFamily(ctx context.Context, w http.ResponseWriter, familyID, clientID string) {
	err := s.deleteRecord(ctx, KindFamily, familyID)
	if err != nil {
		s.serverError(ctx, w, err, "the refresh token could not be checked")
		return
	}
	s.report(ctx, ez.New(ez.ENOTAUTHORIZED, "oauth: refresh token reuse detected, family "+familyID+" of client "+clientID+" revoked", nil))
	writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid, expired or already used")
}

// lostRotation answers a refresh of the token id whose Swap found the family
// row gone or changed since it was read, after reading it again. Gone means
// the family was revoked or expired meanwhile. Naming another refresh token
// means a concurrent request presented the same token and won the Swap: the
// token was presented twice, so the family is revoked. The rows this request
// wrote stay inert either way.
func (s *Server) lostRotation(ctx context.Context, w http.ResponseWriter, id, familyID, clientID string) {
	family, _, err := s.getFamily(ctx, familyID)
	if err != nil && ez.ErrorCode(err) == ez.ENOTFOUND {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid, expired or already used")
		return
	}
	if err != nil {
		s.serverError(ctx, w, err, "the refresh token could not be checked")
		return
	}
	if family.Refresh == id {
		// No write of this package changes a family row and keeps its
		// refresh token, so the Store is not behaving as the contract says.
		s.serverError(ctx, w, ez.New(ez.EINTERNAL, "oauth: the family record changed without a rotation", nil), "the refresh token could not be rotated")
		return
	}
	s.revokeFamily(ctx, w, familyID, clientID)
}

// committed reports whether the family row names refresh as its current
// token, which only the Swap that published it writes. It is asked after a
// Swap failed with anything but ENOTFOUND: the Swap may have committed before
// running out of time. It reads on its own deadline and fails closed, so a
// second failure is answered server_error and the client's retry is reuse.
func (s *Server) committed(ctx context.Context, familyID, refresh string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.RefreshTimeout)
	defer cancel()
	family, _, err := s.getFamily(ctx, familyID)
	if err != nil {
		s.report(ctx, err)
		return false
	}
	return family.Refresh == refresh
}

// tokenPair is an access and a refresh token stored for a family, with the
// lifetime of the access token.
type tokenPair struct {
	access    string
	refresh   string
	expiresIn time.Duration
}

// putTokens generates an access and a refresh token for the generation of
// family and stores both rows, pointing at familyID. The rows are inert until
// the family row exists at that generation, so the caller commits by writing
// the family afterwards, and a failure here leaves nothing usable behind.
// The refresh token expires with the family as it is now, and keeps that
// expiry once rotated; the access token after AccessTokenTTL or with the
// family, whichever comes first.
func (s *Server) putTokens(ctx context.Context, familyID string, family familyRecord) (tokenPair, error) {
	access, err := token.Generate(s.cfg.TokenPrefix + "at")
	if err != nil {
		return tokenPair{}, ez.Wrap(err)
	}
	refresh, err := token.Generate(s.cfg.TokenPrefix + "rt")
	if err != nil {
		return tokenPair{}, ez.Wrap(err)
	}
	now := time.Now()
	accessTTL := min(s.cfg.AccessTokenTTL, family.ExpiresAt.Sub(now))
	row := tokenRecord{Family: familyID, Generation: family.Generation}
	err = s.putRecord(ctx, KindAccess, hashID(access.Value), row, now.Add(accessTTL))
	if err != nil {
		return tokenPair{}, ez.Wrap(err)
	}
	err = s.putRecord(ctx, KindRefresh, hashID(refresh.Value), row, family.ExpiresAt)
	if err != nil {
		return tokenPair{}, ez.Wrap(err)
	}
	return tokenPair{access: access.Value, refresh: refresh.Value, expiresIn: accessTTL}, nil
}

// writeTokens answers with the token response of pair, issued for family.
func writeTokens(w http.ResponseWriter, pair tokenPair, family familyRecord) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken:  pair.access,
		TokenType:    "Bearer",
		ExpiresIn:    int64(pair.expiresIn / time.Second),
		RefreshToken: pair.refresh,
		Scope:        family.responseScope(),
	})
}

// getFamily returns the family row under id, decoded, with the exact bytes
// read, which swapFamily needs as the value it expects. A missing or expired
// family is ez.ENOTFOUND, a failing Store ez.EINTERNAL.
func (s *Server) getFamily(ctx context.Context, id string) (familyRecord, []byte, error) {
	raw, err := s.readRecord(ctx, KindFamily, id, false)
	if err != nil {
		return familyRecord{}, nil, ez.Wrap(err)
	}
	var family familyRecord
	err = json.Unmarshal(raw, &family)
	if err != nil {
		return familyRecord{}, nil, ez.New(ez.EINTERNAL, "oauth: decoding a family record failed", err)
	}
	return family, raw, nil
}

// swapFamily stores next as the family row under id only while the row still
// holds old, the bytes getFamily read: of two rotations from the same read,
// one wins. A row that is gone or changed is ez.ENOTFOUND, a failing Store
// ez.EINTERNAL.
func (s *Server) swapFamily(ctx context.Context, id string, old []byte, next familyRecord) error {
	data, err := json.Marshal(next)
	if err != nil {
		return ez.New(ez.EINTERNAL, "oauth: encoding a family record failed", err)
	}
	err = s.cfg.Store.Swap(ctx, KindFamily, id, old, data, next.ExpiresAt)
	if err != nil && ez.ErrorCode(err) == ez.ENOTFOUND {
		return ez.Wrap(err)
	}
	if err != nil {
		// Whatever code the Store uses, a failing Store is an internal error
		// here, distinct from the codes the handlers show to users.
		return ez.New(ez.EINTERNAL, "oauth: swapping the family record failed", err)
	}
	return nil
}

// isAccessToken reports whether value has the shape of an access token of
// this server. Other values never reach the Store.
func (s *Server) isAccessToken(value string) bool {
	return token.Check(s.cfg.TokenPrefix+"at", value)
}

// accessGrant returns the family of a live access token. The token row only
// points at its family, and the token lives only while the family row does,
// so deleting a family revokes its access tokens at once. An access token
// issued before a rotation stays valid until it expires; one of a later
// generation than the family's was written by a rotation that has not
// committed, and is refused. A row a failed rotation left behind is accepted
// once a retry commits its generation: it was never sent to a client, so
// nothing is exposed. Unknown, expired or revoked tokens yield ez.ENOTFOUND;
// a failing Store, for the token or the family, ez.EINTERNAL.
func (s *Server) accessGrant(ctx context.Context, accessToken string) (familyRecord, error) {
	var record tokenRecord
	err := s.getRecord(ctx, KindAccess, hashID(accessToken), false, &record)
	if err != nil {
		return familyRecord{}, ez.Wrap(err)
	}
	family, _, err := s.getFamily(ctx, record.Family)
	if err != nil && ez.ErrorCode(err) == ez.ENOTFOUND {
		return familyRecord{}, ez.New(ez.ENOTFOUND, "oauth: the access token family is revoked or expired", nil)
	}
	if err != nil {
		return familyRecord{}, ez.Wrap(err)
	}
	if record.Generation > family.Generation {
		// Written by a rotation that has not committed, or never will. A
		// leftover of a failed rotation is accepted once a retry commits
		// its generation; it was never sent to a client.
		return familyRecord{}, ez.New(ez.ENOTFOUND, "oauth: the access token was never published", nil)
	}
	return family, nil
}

// Verify resolves a live access token to its grant. Unknown, malformed,
// expired or revoked tokens return ez.ENOTAUTHENTICATED. Store failures
// return ez.EINTERNAL.
func (s *Server) Verify(ctx context.Context, accessToken string) (Grant, error) {
	if !s.isAccessToken(accessToken) {
		return Grant{}, ez.New(ez.ENOTAUTHENTICATED, "oauth: malformed access token", nil)
	}
	family, err := s.accessGrant(ctx, accessToken)
	if err != nil && ez.ErrorCode(err) == ez.ENOTFOUND {
		return Grant{}, ez.New(ez.ENOTAUTHENTICATED, "oauth: unknown or expired access token", nil)
	}
	if err != nil {
		return Grant{}, ez.Wrap(err)
	}
	return family.Grant, nil
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

// verifyToken is the auth.TokenVerifier of Protect. A token shaped like an
// access token is looked up in the Store with accessGrant, so unknown,
// expired and revoked tokens are refused alike; any other token goes to
// Fallback without a Store read. Rejected tokens wrap auth.ErrInvalidToken so the
// middleware answers 401; Store and Fallback failures go to OnError and
// answer 500. The middleware writes the error text as the response body.
func (s *Server) verifyToken(ctx context.Context, bearer string, r *http.Request) (*auth.TokenInfo, error) {
	// The errors returned here are not ez errors: their text reaches the
	// client, and ez adds the operation name to it.
	if s.isAccessToken(bearer) {
		family, err := s.accessGrant(ctx, bearer)
		if err == nil {
			// No Expiration: token rows do not carry theirs, the Store
			// refuses expired ones, and Protect allows a missing expiry.
			return &auth.TokenInfo{UserID: family.Grant.Subject}, nil
		}
		if ez.ErrorCode(err) != ez.ENOTFOUND {
			// The middleware writes the error text to the response, so the
			// Store error goes to OnError and stays out of it.
			s.report(ctx, err)
			return nil, errors.New("the access token could not be checked")
		}
		return nil, fmt.Errorf("unknown or expired access token: %w", auth.ErrInvalidToken)
	}
	if s.cfg.Fallback == nil {
		return nil, fmt.Errorf("unknown or expired access token: %w", auth.ErrInvalidToken)
	}
	accepted, err := s.cfg.Fallback(ctx, bearer)
	if err != nil {
		s.report(ctx, err)
		return nil, errors.New("the bearer token could not be checked")
	}
	if !accepted {
		return nil, fmt.Errorf("the bearer token is not accepted: %w", auth.ErrInvalidToken)
	}
	return &auth.TokenInfo{}, nil
}

// AuthFunc is the mcp.Config.Auth of an MCP server behind this one. It
// returns the header "Authorization: Bearer <token>" for a live access token,
// so the REST API receives the token unchanged and resolves it with Verify.
// An unknown, expired or revoked access token fails with
// ez.ENOTAUTHENTICATED, so the client refreshes. Any other token goes to
// Fallback and, when accepted, is passed through the same way; when refused,
// or without Fallback, it fails with ez.ENOTAUTHENTICATED. A Fallback
// failure goes to OnError and is returned as it is. An empty token yields no
// headers.
func (s *Server) AuthFunc() func(ctx context.Context, token string) (http.Header, error) {
	return func(ctx context.Context, bearer string) (http.Header, error) {
		if bearer == "" {
			return http.Header{}, nil
		}
		passthrough := http.Header{"Authorization": {"Bearer " + bearer}}
		if s.isAccessToken(bearer) {
			_, err := s.Verify(ctx, bearer)
			if err != nil {
				if ez.ErrorCode(err) != ez.ENOTAUTHENTICATED {
					s.report(ctx, err)
				}
				return nil, ez.Wrap(err)
			}
			return passthrough, nil
		}
		if s.cfg.Fallback == nil {
			return nil, ez.New(ez.ENOTAUTHENTICATED, "oauth: unknown or expired access token", nil)
		}
		accepted, err := s.cfg.Fallback(ctx, bearer)
		if err != nil {
			s.report(ctx, err)
			return nil, ez.Wrap(err)
		}
		if !accepted {
			return nil, ez.New(ez.ENOTAUTHENTICATED, "oauth: the bearer token is not accepted", nil)
		}
		return passthrough, nil
	}
}
