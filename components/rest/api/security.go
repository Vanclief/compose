package api

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
)

// Security scheme types.
const (
	apiKeyType = "apiKey"
	httpType   = "http"
)

// schemeNamePattern is what OpenAPI allows as the key of a component.
var schemeNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// SecurityScheme is a credential the document tells clients to send: an API
// key in a request header, or an http credential such as a bearer token. It
// has the fields of generator.SecurityScheme, so a generator config moves to
// Security unchanged.
type SecurityScheme struct {
	// Name is the scheme's key under components.securitySchemes. It must
	// match [A-Za-z0-9._-]+ and be unique.
	Name string
	// Type is "apiKey" (the default when empty, header named by Header) or
	// "http".
	Type string
	// Header names the header of an apiKey scheme. Required for one, and
	// refused on an http scheme.
	Header string
	// Scheme is the http scheme, "bearer". Required when Type is "http", and
	// refused on an apiKey scheme.
	Scheme string
	// BearerFormat is a hint for clients, for example "opaque". Refused on
	// an apiKey scheme.
	BearerFormat string
	// Description tells clients how to obtain and send the credential.
	Description string
}

// Security declares the document's security schemes and what every operation
// not marked Public requires of them: every apiKey scheme together, in one
// requirement, or any one http scheme alone, each an alternative listed
// after it in the order given. So an API key and its secret are declared as
// two apiKey schemes, and an app that takes that pair or a bearer token
// adds an http scheme:
//
//	api.Security(
//		api.SecurityScheme{Name: "ApiKeyAuth", Header: "API-KEY"},
//		api.SecurityScheme{Name: "ApiSecretAuth", Header: "API-SECRET"},
//		api.SecurityScheme{Name: "BearerAuth", Type: "http", Scheme: "bearer"},
//	)
//
// is published as security: [{ApiKeyAuth: [], ApiSecretAuth: []},
// {BearerAuth: []}]. It only writes the document: the app's Authenticate
// function checks the credentials, and must accept what is declared here.
// Registry option. New panics when it is passed twice or with APIKeyHeader.
// Panics when no scheme is given, a Name is empty, repeated or does not
// match [A-Za-z0-9._-]+, a Type is not "apiKey" or "http", an apiKey scheme
// has no Header or one another scheme names, an http scheme has no Scheme,
// or a scheme sets a field its type does not use.
func Security(schemes ...SecurityScheme) Option {
	schemes = slices.Clone(schemes)
	err := checkSecurity(schemes)
	if err != nil {
		panic(ez.New(ez.EINVALID, "api: Security: "+ez.ErrorMessage(err), err))
	}
	return Option{name: "api.Security", registry: func(r *Registry) {
		if r.security != nil {
			panic(ez.New(ez.EINVALID, "api: New: api.Security is passed twice: declare every scheme in one call", nil))
		}
		r.security = schemes
	}}
}

// checkSecurity validates the schemes of Security and sets the Type of the
// ones that leave it empty.
func checkSecurity(schemes []SecurityScheme) error {
	if len(schemes) == 0 {
		return ez.New(ez.EINVALID, "needs a scheme", nil)
	}

	names := map[string]bool{}
	headers := map[string]string{} // lower-cased header -> scheme name
	for i := range schemes {
		s := &schemes[i]
		if !schemeNamePattern.MatchString(s.Name) {
			return ez.New(ez.EINVALID, fmt.Sprintf("scheme name %q must match %s", s.Name, schemeNamePattern), nil)
		}
		if names[s.Name] {
			return ez.New(ez.EINVALID, fmt.Sprintf("scheme %s is declared twice", s.Name), nil)
		}
		names[s.Name] = true

		switch s.Type {
		case "", apiKeyType:
			s.Type = apiKeyType
			if s.Header == "" {
				return ez.New(ez.EINVALID, fmt.Sprintf("scheme %s of type apiKey needs a Header", s.Name), nil)
			}
			if s.Scheme != "" || s.BearerFormat != "" {
				return ez.New(ez.EINVALID, fmt.Sprintf("scheme %s of type apiKey sets Scheme or BearerFormat, which only an http scheme has: set Type to \"http\" or drop them", s.Name), nil)
			}
			header := strings.ToLower(s.Header)
			other, taken := headers[header]
			if taken {
				return ez.New(ez.EINVALID, fmt.Sprintf("schemes %s and %s name the same header %s", other, s.Name, s.Header), nil)
			}
			headers[header] = s.Name
		case httpType:
			if s.Scheme == "" {
				return ez.New(ez.EINVALID, fmt.Sprintf("scheme %s of type http needs a Scheme", s.Name), nil)
			}
			if s.Header != "" {
				return ez.New(ez.EINVALID, fmt.Sprintf("scheme %s of type http sets Header, which only an apiKey scheme has: an http credential is sent in Authorization", s.Name), nil)
			}
		default:
			return ez.New(ez.EINVALID, fmt.Sprintf("scheme %s has unknown type %q: use \"apiKey\" or \"http\"", s.Name, s.Type), nil)
		}
	}
	return nil
}

// securitySchemes returns components.securitySchemes.
func (r *Registry) securitySchemes() map[string]*huma.SecurityScheme {
	out := map[string]*huma.SecurityScheme{}
	for _, s := range r.security {
		scheme := &huma.SecurityScheme{Type: s.Type, Description: s.Description}
		if s.Type == httpType {
			scheme.Scheme = s.Scheme
			scheme.BearerFormat = s.BearerFormat
		} else {
			scheme.In = "header"
			scheme.Name = s.Header
		}
		out[s.Name] = scheme
	}
	return out
}

// securityRequirement returns the security of a protected operation: apiKey
// schemes are sent together, so they share one requirement, and each http
// scheme is an alternative credential with its own, after it.
func (r *Registry) securityRequirement() []map[string][]string {
	keys := map[string][]string{}
	var alternatives []map[string][]string
	for _, s := range r.security {
		if s.Type == httpType {
			alternatives = append(alternatives, map[string][]string{s.Name: {}})
			continue
		}
		keys[s.Name] = []string{}
	}
	if len(keys) == 0 {
		return alternatives
	}
	return append([]map[string][]string{keys}, alternatives...)
}
