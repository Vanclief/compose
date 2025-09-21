package handler

import (
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/vanclief/ez"
)

// Parameters
func (h *BaseHandler) GetParameterString(c echo.Context, name string) (string, error) {
	const op = "BaseHandler.GetParameterString"

	paramStr := c.Param(name)
	if paramStr == "" {
		errMsg := fmt.Sprintf("Parameter %s is required", name)
		return "", ez.New(op, ez.EINVALID, errMsg, nil)
	}

	return paramStr, nil
}

func (h *BaseHandler) GetParameterInt64(c echo.Context, name string) (int64, error) {
	const op = "BaseHandler.GetParameterInt64"

	int64Str, err := h.GetParameterString(c, name)
	if err != nil {
		return 0, ez.Wrap(op, err)
	}

	int64, err := strconv.ParseInt(int64Str, 10, 64)
	if err != nil {
		return 0, ez.New(op, ez.EINVALID, "Could not parse parameter to int", err)
	}

	return int64, nil
}

func (h *BaseHandler) GetParameterUUID(c echo.Context, name string) (uuid.UUID, error) {
	const op = "BaseHandler.GetParameterUUID"

	uuidStr, err := h.GetParameterString(c, name)
	if err != nil {
		return uuid.Nil, ez.Wrap(op, err)
	}

	// parse string to uuid
	id, err := uuid.Parse(uuidStr)
	if err != nil {
		return uuid.Nil, ez.New(op, ez.EINVALID, "Could not parse parameter to UUID", err)
	}

	return id, nil
}

// QueryParams

func (h *BaseHandler) GetQueryParamInt64(c echo.Context, value string) (int64, error) {
	const op = "BaseHandler.GetQueryParamInt64"

	int64Str := c.QueryParam(value)
	if int64Str == "" {
		errMsg := fmt.Sprintf("Query param %s is required", value)
		return 0, ez.New(op, ez.EINVALID, errMsg, nil)
	}

	int64, err := strconv.ParseInt(int64Str, 10, 64)
	if err != nil {
		return 0, ez.New(op, ez.EINVALID, "Could not parse query param to int", err)
	}

	return int64, nil
}

func (h *BaseHandler) GetQueryParamInt64s(c echo.Context, key string) ([]int64, error) {
	const op = "BaseHandler.GetQueryParamInt64s"

	params := c.QueryParams()[key]

	ints := []int64{}
	for _, param := range params {
		id, err := strconv.ParseInt(param, 10, 64)
		if err != nil {
			return nil, ez.New(op, ez.EINVALID, "Could not parse query params to int", err)
		}
		ints = append(ints, id)
	}

	return ints, nil
}

// GetQueryParamTime parses a required query param as RFC3339/RFC3339Nano
func (h *BaseHandler) GetQueryParamTime(c echo.Context, key string) (time.Time, error) {
	const op = "BaseHandler.GetQueryParamTime"

	dateStr := c.QueryParam(key)
	if dateStr == "" {
		errMsg := fmt.Sprintf("Query param %s is required", key)
		return time.Time{}, ez.New(op, ez.EINVALID, errMsg, nil)
	}

	t, err := time.Parse(time.RFC3339, dateStr)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, dateStr)
		if err != nil {
			errMsg := fmt.Sprintf("Could not parse %s as RFC3339/RFC3339Nano", key)
			return time.Time{}, ez.New(op, ez.EINVALID, errMsg, err)
		}
	}

	return t, nil
}

func (h *BaseHandler) GetListLimit(c echo.Context, defaultLimit int) int {
	return h.GetNumericQueryParam(c, "limit", defaultLimit)
}

func (h *BaseHandler) GetListOffset(c echo.Context, defaultOffset int) int {
	return h.GetNumericQueryParam(c, "offset", defaultOffset)
}

func (h *BaseHandler) GetNumericQueryParam(c echo.Context, param string, defaultValue int) int {
	str := c.QueryParam(param)
	if str == "" {
		return defaultValue
	}

	value, err := strconv.Atoi(str)
	if err != nil {
		return defaultValue
	}

	if value < 0 {
		return defaultValue
	}

	return value
}
