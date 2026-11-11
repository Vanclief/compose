package rest

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/ez"
)

// WithErrorReporter has ErrorHandler call fn with every error it answers,
// ez or not, client or server error, before the answer is written; fn
// decides what to report. handler.ReportToSentry is one, for an app that
// reports to Sentry. The api registry's Ready also calls it once at boot,
// with an ez EINVALID error and handler.Request(c) nil, which
// handler.ReportToSentry skips as a client error. The context of the request
// a registry operation built (handler.Request(c).GetContext()) is already
// cancelled when fn runs: fn reads values from it and does I/O with
// c.Request().Context(). ErrorHandler option. Panics when fn is nil.
func WithErrorReporter(fn func(c *echo.Context, err error)) Option {
	if fn == nil {
		panic(ez.New(ez.EINVALID, "rest: WithErrorReporter needs a function", nil))
	}
	return func(c *config) {
		c.errorReporter = fn
	}
}

// WithErrorTranslator has ErrorHandler apply fn to every ez error right
// before writing it, so an app can localize error messages. Only the
// translated error's message is used; the code and status always come from
// the original error, and returning nil keeps it. Errors that are not ez
// errors are never translated. It is handler.BaseHandler.ErrorTranslator for
// the errors ErrorHandler answers. An app that localizes by the request's
// locale reads handler.Request(c), which is nil for an error raised outside
// a registry operation or handler.ManageError (an unknown route, 405, a panic
// before the handler, the ez EINVALID error the api registry's Ready answers
// once at boot). That request's context is already cancelled when fn runs,
// so fn reads values from it and does I/O with c.Request().Context().
// ErrorHandler option. Panics when fn is nil.
func WithErrorTranslator(fn func(c *echo.Context, err error) error) Option {
	if fn == nil {
		panic(ez.New(ez.EINVALID, "rest: WithErrorTranslator needs a function", nil))
	}
	return func(c *config) {
		c.errorTranslator = fn
	}
}

// ErrorHandler is an Echo HTTPErrorHandler that answers every error returned
// to Echo in compose's handler.ErrorResponse shape: the errors of the api
// registry's operations, which return them rather than writing them, and the
// errors that never reach handler.ManageError (unknown routes, 405,
// recovered panics, errors a handler or middleware returns straight to Echo).
// An app installs it with e.HTTPErrorHandler = rest.ErrorHandler(opts...);
// Configure and Start do not, and the api registry's Ready panics while
// Echo's default handler is installed. It uses WithErrorReporter and
// WithErrorTranslator and ignores the other options, so an app passes it the
// same options as Configure and Serve.
//
// The error is an ez error when it is an *ez.Error itself, or when an
// *ez.Error is in its chain and the chain carries no Echo status
// (echo.StatusCode is 0), the rule handler.ReportToSentry classifies by too.
// An ez error is answered with ez.ErrorToHTTPStatus, ez.ErrorCode and
// ez.ErrorMessage of the error, as handler.ManageError does; an ez error
// wrapped in another kind of error, such as a recovered panic of one, is
// therefore a 500 EINTERNAL with ez's generic message (a 499 or 504 when a
// cancelled or timed-out context is in its chain). An Echo error wrapping
// an ez error, as c.Bind makes when a field's UnmarshalJSON returns one and
// middleware.KeyAuth when its validator does, is not an ez error. Any other
// error with the non-zero StatusCode of an echo.HTTPStatusCoder in the error
// chain gets that status, as Echo's default handler has it; the code is
// ez.HTTPStatusToError of that status; the message is the non-empty Message
// of an *echo.HTTPError in the chain, else http.StatusText of the status. An
// error with neither is classified with ez.ErrorCode, as handler.ManageError
// does: context.Canceled in its chain is a 499 ECANCELED and a timeout
// (context.DeadlineExceeded or os.ErrDeadlineExceeded in its chain, or an
// error whose own Timeout method reports true, such as an HTTP client's) a
// 504 ETIMEOUT, both with ez.ErrorMessage's fixed text, and anything else a
// 500 EINTERNAL with http.StatusText(500). The
// message is never such an error's own text or its wrapped cause, so no
// internal error or panic stack reaches the client. An
// *echo.BindingError embeds the HTTPError without being one, so a typed
// binder's error returned straight to Echo gets the status text, the way
// handler.BindedJSONResponse keeps binding detail out of its answer too. The
// request_id is the X-Request-Id of the response: under Configure the
// request's id, the one the request log line carries, and otherwise whatever
// the app's own middleware set there, which Echo's middleware.RequestID copies
// from the client. The request's own X-Request-Id is never read. A HEAD
// request gets the status with no body.
//
// A response already committed is left alone, hooks included. That makes it
// run once per request under Configure, whose request logger calls it and
// then returns the error, so Echo calls it again on the answered response.
//
// It logs nothing about the error, because the request logger that Configure
// (and so Start) installs has it answer every error raised inside its stack
// and then logs the error under the same id with the status sent, at warn
// level for a 4xx and error level for a 5xx, including the stack of a
// recovered panic, which Echo's *middleware.PanicStackError carries in its
// Error text. An error from middleware outside that stack, registered with
// e.Pre or with e.Use before Configure, which under Start is all of an app's
// middleware, is answered with an empty request_id and logged by nothing;
// Configure then Serve, with the app's e.Use calls in between, puts that
// middleware inside. In an app that runs neither Configure nor Start nothing
// logs a 500, so adopt the two together. Only a failure to write the answer
// is logged, through c.Logger(), as Echo's default handler does.
func ErrorHandler(opts ...Option) echo.HTTPErrorHandler {
	cfg := newConfig(opts...)
	return func(c *echo.Context, err error) {
		res, _ := echo.UnwrapResponse(c.Response())
		if res != nil && res.Committed {
			return
		}

		var status int
		var code string
		var message string
		var ezErr *ez.Error
		_, isEZ := err.(*ez.Error)
		if !isEZ {
			isEZ = errors.As(err, &ezErr) && echo.StatusCode(err) == 0
		}
		if isEZ {
			status = ez.ErrorToHTTPStatus(err)
			code = ez.ErrorCode(err)
			message = ez.ErrorMessage(err)
		} else {
			status = echo.StatusCode(err)
			if status != 0 {
				code = ez.HTTPStatusToError(status)
				message = http.StatusText(status)
			} else {
				// Classified as handler.ManageError does: a cancelled or
				// timed-out context is a 499 or 504 with ez's fixed text,
				// anything else a 500.
				status = ez.ErrorToHTTPStatus(err)
				code = ez.ErrorCode(err)
				message = http.StatusText(status)
				if code != ez.EINTERNAL {
					message = ez.ErrorMessage(err)
				}
			}

			// The binder's 415 is an *echo.HTTPError with an empty Message.
			var httpErr *echo.HTTPError
			if errors.As(err, &httpErr) && httpErr.Message != "" {
				message = httpErr.Message
			}
		}

		if cfg.errorReporter != nil {
			cfg.errorReporter(c, err)
		}

		if isEZ && cfg.errorTranslator != nil {
			translated := cfg.errorTranslator(c, err)
			if translated != nil {
				message = ez.ErrorMessage(translated)
			}
		}

		// The error is answered: err now holds the outcome of the write.
		if c.Request().Method == http.MethodHead {
			err = c.NoContent(status)
		} else {
			stdErr := handler.StandardError{
				Code:      code,
				Message:   message,
				RequestID: requestID(c),
			}
			err = c.JSON(status, handler.ErrorResponse{Error: stdErr})
		}
		if err != nil {
			c.Logger().Error("rest: error handler failed to send the error to the client", "error", err)
		}
	}
}

// requestID returns the X-Request-Id of the response, which the server set:
// under Configure the id of the request. The request's own X-Request-Id is
// not read, since outside Configure it is whatever the client sent. It is
// empty when nothing set the header, as when no request ID middleware ran.
func requestID(c *echo.Context) string {
	return c.Response().Header().Get(echo.HeaderXRequestID)
}
