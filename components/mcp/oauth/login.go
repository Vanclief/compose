package oauth

import (
	"bytes"
	"context"
	"embed"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/vanclief/ez"
)

//go:embed templates/login.html
var loginFS embed.FS

var loginTemplate = template.Must(template.ParseFS(loginFS, "templates/login.html"))

// loginPage is the view model of the login page. With Heading set the page
// shows only Heading and Message.
type loginPage struct {
	Title           string
	Heading         string
	Message         string
	ClientName      string
	RedirectHost    string
	RequestID       string
	IdentifierLabel string
	CredentialLabel string
	// Identifier is the submitted identifier, shown read-only on the
	// credential step of a challenge login and kept in the field otherwise.
	Identifier string
	// AskIdentifier and AskCredential select the fields of the form.
	AskIdentifier bool
	AskCredential bool
	// OneTimeCode marks the credential as a code rather than a password.
	OneTimeCode bool
	Error       string
}

// serveLoginPage shows the form of a pending request.
func (s *Server) serveLoginPage(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("request")
	var request authRequest
	err := s.getRecord(r.Context(), KindRequest, id, false, &request)
	if err != nil {
		s.renderRequestError(w, r, err)
		return
	}
	s.renderForm(w, http.StatusOK, id, request, request.Identifier, "")
}

// serveLoginSubmit handles both steps of the form. The request is taken from
// the Store while it is checked and put back unless it is approved or voided,
// so parallel submissions cannot exceed the attempt limit.
func (s *Server) serveLoginSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PostFormValue("request")
	var request authRequest
	err := s.getRecord(ctx, KindRequest, id, true, &request)
	if err != nil {
		s.renderRequestError(w, r, err)
		return
	}

	login := s.cfg.Login
	identifier := strings.TrimSpace(r.PostFormValue("identifier"))
	if request.Identifier != "" {
		identifier = request.Identifier
	}
	credential := r.PostFormValue("credential")
	if identifier == "" {
		s.keepRequest(w, r, id, request, "", "Enter your "+strings.ToLower(login.IdentifierLabel)+".")
		return
	}

	if login.Challenge != nil && request.Identifier == "" {
		err = login.Challenge(ctx, identifier)
		if err != nil {
			s.failAttempt(w, r, id, request, identifier, s.attemptMessage(ctx, err, ez.EINVALID, ez.ENOTFOUND, ez.ERESOURCEEXHAUSTED))
			return
		}
		request.Identifier = identifier
		s.keepRequest(w, r, id, request, identifier, "")
		return
	}

	if credential == "" {
		s.keepRequest(w, r, id, request, identifier, "Enter your "+strings.ToLower(login.CredentialLabel)+".")
		return
	}
	grant, err := login.Verify(ctx, identifier, credential)
	if err != nil {
		s.failAttempt(w, r, id, request, identifier, s.attemptMessage(ctx, err, ez.ENOTAUTHENTICATED, ez.ENOTFOUND, ez.EINVALID))
		return
	}

	redirect, err := s.approve(ctx, request, grant)
	if err != nil {
		s.renderFailure(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// failAttempt counts a failed Challenge or Verify: the request is voided at
// Config.MaxLoginAttempts, else it is put back and the form shows message.
func (s *Server) failAttempt(w http.ResponseWriter, r *http.Request, id string, request authRequest, identifier, message string) {
	request.Attempts++
	if request.Attempts >= s.cfg.MaxLoginAttempts {
		s.renderExpired(w)
		return
	}
	s.keepRequest(w, r, id, request, identifier, message)
}

// attemptMessage is what the form shows for a failed Challenge or Verify:
// the error message when its code is one of shown, a generic text otherwise.
// Hidden errors go to OnError.
func (s *Server) attemptMessage(ctx context.Context, err error, shown ...string) string {
	if slices.Contains(shown, ez.ErrorCode(err)) {
		return ez.ErrorMessage(err)
	}
	s.report(ctx, err)
	return "Something went wrong. Try again."
}

// keepRequest puts the request back and shows the form again with message.
// grug: the request keeps the expiry set when it was created, so no
// resubmission extends its RequestTTL window, not even the empty-field ones
// that count no attempt.
func (s *Server) keepRequest(w http.ResponseWriter, r *http.Request, id string, request authRequest, identifier, message string) {
	err := s.putRecord(r.Context(), KindRequest, id, request, request.ExpiresAt)
	if err != nil {
		s.renderFailure(w, r, err)
		return
	}
	s.renderForm(w, http.StatusOK, id, request, identifier, message)
}

// renderForm shows the step of the form the request is at.
func (s *Server) renderForm(w http.ResponseWriter, status int, id string, request authRequest, identifier, message string) {
	login := s.cfg.Login
	challenged := login.Challenge != nil && request.Identifier != ""
	page := loginPage{
		Title:           login.Title,
		ClientName:      request.ClientName,
		RedirectHost:    redirectHost(request.RedirectURI),
		RequestID:       id,
		IdentifierLabel: login.IdentifierLabel,
		CredentialLabel: login.CredentialLabel,
		Identifier:      identifier,
		AskIdentifier:   !challenged,
		AskCredential:   login.Challenge == nil || challenged,
		OneTimeCode:     login.Challenge != nil,
		Error:           message,
	}
	s.render(w, status, page)
}

// redirectHost is the host the browser goes back to, shown so the user can
// tell where the grant ends up.
func redirectHost(redirectURI string) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return redirectURI
	}
	return u.Host
}

// renderRequestError shows the expired page for unknown requests and a
// generic error otherwise.
func (s *Server) renderRequestError(w http.ResponseWriter, r *http.Request, err error) {
	if ez.ErrorCode(err) == ez.ENOTFOUND {
		s.renderExpired(w)
		return
	}
	s.renderFailure(w, r, err)
}

// renderFailure reports err and shows the generic error page.
func (s *Server) renderFailure(w http.ResponseWriter, r *http.Request, err error) {
	s.report(r.Context(), err)
	s.renderMessage(w, http.StatusInternalServerError, "Something went wrong", "Start again from the application.")
}

// renderExpired shows the page for a request that is gone.
func (s *Server) renderExpired(w http.ResponseWriter) {
	s.renderMessage(w, http.StatusBadRequest, "Login link expired", "This login link has expired. Start again from the application.")
}

// renderMessage shows a page with a heading and a message only.
func (s *Server) renderMessage(w http.ResponseWriter, status int, heading, message string) {
	s.render(w, status, loginPage{Title: s.cfg.Login.Title, Heading: heading, Message: message})
}

// render writes the page. It is never cached nor framed.
func (s *Server) render(w http.ResponseWriter, status int, page loginPage) {
	var out bytes.Buffer
	err := loginTemplate.Execute(&out, page)
	if err != nil {
		http.Error(w, "oauth: rendering the login page failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.WriteHeader(status)
	w.Write(out.Bytes())
}
