package rest

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/ez"
)

// ErrorHandler is an Echo HTTPErrorHandler that answers the errors that never
// reach handler.ManageError (unknown routes, 405, recovered panics, errors a
// handler or middleware returns straight to Echo) in compose's
// handler.ErrorResponse shape. An app installs it with
// e.HTTPErrorHandler = rest.ErrorHandler(); Configure and Start do not.
//
// The status is the non-zero StatusCode of an echo.HTTPStatusCoder in the
// error chain, else 500, as Echo's default handler and the request log have
// it. The code is ez.HTTPStatusToError of that status. The message is the
// non-empty Message of an *echo.HTTPError in the chain, else
// http.StatusText of the status; it is never the error's own text or its
// wrapped cause, so no internal error or panic stack reaches the client. An
// *echo.BindingError embeds the HTTPError without being one, so a typed
// binder's error returned straight to Echo gets the status text, the way
// handler.BindedJSONResponse keeps binding detail out of its answer too. The
// request_id is the X-Request-Id of the response: under Configure the
// request's id, the one the request log line carries, and otherwise whatever
// the app's own middleware set there, which Echo's middleware.RequestID copies
// from the client. The request's own X-Request-Id is never read. A HEAD
// request gets the status with no body. A response already committed is left
// alone.
//
// It logs nothing about the error, because the request logger that Configure
// (and so Start) installs logs every error raised inside its stack at error
// level under the same id, including the stack of a recovered panic, which
// Echo's *middleware.PanicStackError carries in its Error text. An error
// from middleware outside that stack, registered with e.Pre or with e.Use
// before Configure, which under Start is all of an app's middleware, is
// answered with an empty request_id and logged by nothing; Configure then
// Serve, with the app's e.Use calls in between, puts that middleware inside.
// In an app that runs neither Configure nor Start nothing logs a 500, so
// adopt the two together. Only a failure to write the answer is logged,
// through c.Logger(), as Echo's default handler does.
func ErrorHandler() echo.HTTPErrorHandler {
	return func(c *echo.Context, err error) {
		res, _ := echo.UnwrapResponse(c.Response())
		if res != nil && res.Committed {
			return
		}

		status := echo.StatusCode(err)
		if status == 0 {
			status = http.StatusInternalServerError
		}

		// The binder's 415 is an *echo.HTTPError with an empty Message.
		message := http.StatusText(status)
		var httpErr *echo.HTTPError
		if errors.As(err, &httpErr) && httpErr.Message != "" {
			message = httpErr.Message
		}

		if c.Request().Method == http.MethodHead {
			err = c.NoContent(status)
		} else {
			stdErr := handler.StandardError{
				Code:      ez.HTTPStatusToError(status),
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
