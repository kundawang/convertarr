package handler

import (
	"encoder/app"
	"net/http"

	"github.com/labstack/echo/v4"
)

// PostEncodingCancel cancels the currently running encode (if any).
// The encode aborts at the current pass, the history entry is marked as
// failed and half-finished artifacts (partial output, passlog files) are
// cleaned up by the encoder.
func PostEncodingCancel(c echo.Context) error {
	app.CancelCurrentEncoding()
	return c.Redirect(http.StatusFound, "/history")
}
