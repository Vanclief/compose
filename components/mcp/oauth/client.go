package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/vanclief/ez"
)

const (
	// maxDocumentBytes bounds a metadata document and a registration body.
	maxDocumentBytes = 64 << 10
	// errDocumentFetch is the message of every failure to fetch a metadata
	// document.
	errDocumentFetch = "oauth: the client metadata document could not be fetched"
)

// checkClient validates a client listed in Config.Clients.
func checkClient(client Client) error {
	if client.ID == "" || client.Name == "" {
		return ez.New(ez.EINVALID, "oauth: every Config.Clients entry needs an ID and a Name", nil)
	}
	if len(client.RedirectURIs) == 0 {
		return ez.New(ez.EINVALID, "oauth: client "+client.ID+" needs at least one redirect URI", nil)
	}
	for _, uri := range client.RedirectURIs {
		if !validRedirectURI(uri) {
			return ez.New(ez.EINVALID, "oauth: client "+client.ID+" has an invalid redirect URI "+strconv.Quote(uri), nil)
		}
	}
	return nil
}

// validRedirectURI accepts https URIs, and http URIs on a loopback host for
// native clients, without fragment. The host must be ASCII: the login page
// shows it, and a Unicode lookalike of a known host would defeat that.
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" || u.User != nil || !isASCII(u.Host) {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && isLocalHost(u.Hostname())
}

// isASCII reports whether s has no byte outside ASCII.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// errRefusedAddress is the cause of a fetch refused by publicAddressOnly. It
// is a plain sentinel, not an ez error: the error arrives wrapped in a
// url.Error and a net.OpError, and ez.ErrorCode only inspects the top level,
// so errors.Is is the only way to classify it.
var errRefusedAddress = errors.New("oauth: the address is not public")

// knownClient resolves a client_id without fetching anything: Config.Clients
// first, then stored clients (registered or cached documents). Unknown ids
// yield ez.ENOTFOUND.
func (s *Server) knownClient(ctx context.Context, clientID string) (Client, error) {
	if clientID == "" {
		return Client{}, ez.New(ez.ENOTFOUND, "oauth: client_id is missing", nil)
	}
	for _, client := range s.cfg.Clients {
		if client.ID == clientID {
			client.configured = true
			return client, nil
		}
	}
	var client Client
	err := s.getRecord(ctx, KindClient, clientID, false, &client)
	if err != nil {
		return Client{}, ez.Wrap(err)
	}
	return client, nil
}

// renewClient stores a dynamically registered client again with a fresh
// RegisteredClientTTL expiry, so clients in use never expire while dormant
// ones do. client must be the full stored record, as knownClient returns it.
// Clients from Config.Clients are not stored, and cached Client ID Metadata
// Documents keep their MetadataCacheTTL, so both are left alone. A failing
// Store is ez.EINTERNAL.
func (s *Server) renewClient(ctx context.Context, client Client) error {
	if client.configured || isDocumentURL(client.ID) {
		return nil
	}
	err := s.putRecord(ctx, KindClient, client.ID, client, time.Now().Add(s.cfg.RegisteredClientTTL))
	if err != nil {
		return ez.Wrap(err)
	}
	return nil
}

// lookupClient resolves a client_id for the authorize endpoint: knownClient,
// then a Client ID Metadata Document when the id is an https URL. Unknown
// ids yield ez.ENOTFOUND, a document that cannot be reached ez.EUNAVAILABLE,
// an unusable one ez.EINVALID.
func (s *Server) lookupClient(ctx context.Context, clientID string) (Client, error) {
	client, err := s.knownClient(ctx, clientID)
	if err == nil {
		return client, nil
	}
	if ez.ErrorCode(err) != ez.ENOTFOUND {
		return Client{}, ez.Wrap(err)
	}
	if !isDocumentURL(clientID) {
		return Client{}, ez.New(ez.ENOTFOUND, "oauth: unknown client", nil)
	}

	client, err = s.fetchClientDocument(ctx, clientID)
	if err != nil {
		return Client{}, ez.Wrap(err)
	}
	err = s.putRecord(ctx, KindClient, clientID, client, time.Now().Add(s.cfg.MetadataCacheTTL))
	if err != nil {
		return Client{}, ez.Wrap(err)
	}
	return client, nil
}

// isDocumentURL reports whether a client_id names a Client ID Metadata
// Document: an https URL with a path, without query, fragment, user info or
// dot segments.
func isDocumentURL(clientID string) bool {
	u, err := url.Parse(clientID)
	if err != nil {
		return false
	}
	segments := strings.Split(u.Path, "/")
	if u.RawQuery != "" || u.ForceQuery || slices.Contains(segments, ".") || slices.Contains(segments, "..") {
		return false
	}
	return u.Scheme == "https" && u.Host != "" && u.Path != "" && u.Path != "/" && u.User == nil && u.Fragment == ""
}

// reservedPrefixes are the non-public ranges that netip.Addr does not
// classify: "this" network, carrier-grade NAT (Tailscale and some cloud
// metadata services), IETF protocol assignments, benchmarking, class E,
// the NAT64 prefixes that map IPv4 addresses, Teredo and 6to4, which embed
// an IPv4 address.
var reservedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2002::/16"),
}

// publicAddressOnly is the net.Dialer Control of the default HTTP client: it
// refuses connections to loopback, private, link-local, unspecified,
// multicast, unique-local and reservedPrefixes addresses. address is the
// resolved ip:port.
func publicAddressOnly(network, address string, conn syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return ez.New(ez.EINVALID, "oauth: cannot parse the address "+address, err)
	}
	// Unmap turns ::ffff:a.b.c.d into a.b.c.d so the IPv4 rules apply to it.
	ip := addrPort.Addr().Unmap()
	refused := ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast()
	for _, prefix := range reservedPrefixes {
		refused = refused || prefix.Contains(ip)
	}
	if refused {
		return ez.New(ez.EINVALID, "oauth: client metadata documents must be served from a public address", errRefusedAddress)
	}
	return nil
}

// allowedRedirectHosts reports whether every redirect URI host is in
// Config.AllowedRedirectHosts, or the list is empty.
func (s *Server) allowedRedirectHosts(uris []string) bool {
	if len(s.cfg.AllowedRedirectHosts) == 0 {
		return true
	}
	for _, uri := range uris {
		u, err := url.Parse(uri)
		if err != nil || !slices.Contains(s.cfg.AllowedRedirectHosts, u.Hostname()) {
			return false
		}
	}
	return true
}

// clientDocument is the part of a Client ID Metadata Document the server
// uses.
type clientDocument struct {
	ClientID     string   `json:"client_id"`
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// fetchClientDocument downloads and validates the metadata document at
// clientID. A document that cannot be reached (connection failure, timeout,
// 5xx) is ez.EUNAVAILABLE: the user may try again. Everything else,
// including a host that resolves to an address publicAddressOnly refuses, is
// ez.EINVALID: the client cannot be used. Fetch failures share one message
// so the error page does not echo what the upstream answered.
func (s *Server) fetchClientDocument(ctx context.Context, clientID string) (Client, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return Client{}, ez.New(ez.EINVALID, "oauth: client_id is not a fetchable URL", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if errors.Is(err, errRefusedAddress) {
		return Client{}, ez.New(ez.EINVALID, "oauth: client_id resolves to an address this server does not fetch", err)
	}
	if err != nil {
		return Client{}, ez.New(ez.EUNAVAILABLE, errDocumentFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return Client{}, ez.New(ez.EUNAVAILABLE, errDocumentFetch, nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Client{}, ez.New(ez.EINVALID, errDocumentFetch, nil)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes+1))
	if err != nil {
		return Client{}, ez.New(ez.EUNAVAILABLE, errDocumentFetch, err)
	}
	if len(data) > maxDocumentBytes {
		return Client{}, ez.New(ez.EINVALID, "oauth: the client metadata document is larger than 64 KiB", nil)
	}

	var doc clientDocument
	err = json.Unmarshal(data, &doc)
	if err != nil {
		return Client{}, ez.New(ez.EINVALID, "oauth: the client metadata document is not JSON", err)
	}
	if doc.ClientID != clientID {
		return Client{}, ez.New(ez.EINVALID, "oauth: the client metadata document has a different client_id", nil)
	}
	if doc.ClientName == "" || len(doc.RedirectURIs) == 0 {
		return Client{}, ez.New(ez.EINVALID, "oauth: the client metadata document needs client_name and redirect_uris", nil)
	}
	for _, uri := range doc.RedirectURIs {
		if !validRedirectURI(uri) {
			return Client{}, ez.New(ez.EINVALID, "oauth: the client metadata document has an invalid redirect URI", nil)
		}
	}
	if !s.allowedRedirectHosts(doc.RedirectURIs) {
		return Client{}, ez.New(ez.EINVALID, "oauth: the client metadata document has a redirect URI on a host this server does not accept", nil)
	}
	return Client{ID: clientID, Name: doc.ClientName, RedirectURIs: doc.RedirectURIs}, nil
}

// serveRegister is RFC 7591 dynamic client registration.
func (s *Server) serveRegister(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var meta oauthex.ClientRegistrationMetadata
	err := json.NewDecoder(io.LimitReader(r.Body, maxDocumentBytes)).Decode(&meta)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body must be a JSON client metadata object")
		return
	}
	if len(meta.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, uri := range meta.RedirectURIs {
		if !validRedirectURI(uri) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URIs must be https, or http on localhost, without fragment: "+uri)
			return
		}
	}
	if !s.allowedRedirectHosts(meta.RedirectURIs) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URIs must be on a host this server accepts")
		return
	}
	method := meta.TokenEndpointAuthMethod
	if method == "" {
		method = "none"
	}
	if !slices.Contains([]string{"none", "client_secret_basic", "client_secret_post"}, method) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "token_endpoint_auth_method must be none, client_secret_basic or client_secret_post")
		return
	}

	client := Client{ID: randomID(), Name: meta.ClientName, RedirectURIs: meta.RedirectURIs}
	if method != "none" {
		client.Secret = randomID()
	}
	err = s.putRecord(r.Context(), KindClient, client.ID, client, time.Now().Add(s.cfg.RegisteredClientTTL))
	if err != nil {
		s.serverError(r.Context(), w, err, "the client could not be stored")
		return
	}

	response := registrationResponse{
		ClientRegistrationMetadata: oauthex.ClientRegistrationMetadata{
			RedirectURIs:            client.RedirectURIs,
			TokenEndpointAuthMethod: method,
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			ClientName:              client.Name,
		},
		ClientID:         client.ID,
		ClientSecret:     client.Secret,
		ClientIDIssuedAt: time.Now().Unix(),
	}
	if client.Secret != "" {
		never := int64(0)
		response.ClientSecretExpiresAt = &never
	}
	writeJSON(w, http.StatusCreated, response)
}

// registrationResponse is the RFC 7591 registration response.
// oauthex.ClientRegistrationResponse omits client_secret_expires_at when it
// is 0, but the field is required with a secret, and 0 means it never
// expires.
type registrationResponse struct {
	oauthex.ClientRegistrationMetadata
	ClientID              string `json:"client_id"`
	ClientSecret          string `json:"client_secret,omitempty"`
	ClientIDIssuedAt      int64  `json:"client_id_issued_at"`
	ClientSecretExpiresAt *int64 `json:"client_secret_expires_at,omitempty"`
}
