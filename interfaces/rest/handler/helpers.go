package handler

import (
	"fmt"
	"strconv"

	"github.com/labstack/echo/v4"
	"github.com/vanclief/ez"
)

func (h *BaseHandler) GetParameterID(c echo.Context, name string) (int64, error) {
	const op = "BaseHandler.GetParameterID"

	idStr, err := h.GetParameterString(c, name)
	if err != nil {
		return 0, ez.Wrap(op, err)
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, ez.New(op, ez.EINVALID, `Could not parse parameter to int`, err)
	}

	return id, nil
}

func (h *BaseHandler) GetParameterString(c echo.Context, name string) (string, error) {
	const op = "BaseHandler.GetParameterString"

	idStr := c.Param(name)
	if idStr == "" {
		return "", ez.New(op, ez.EINVALID, `Resource ID is required`, nil)
	}

	return idStr, nil
}

func (h *BaseHandler) GetQueryID(c echo.Context, value string) (int64, error) {
	const op = "BaseHandler.GetQueryID"

	idStr := c.QueryParam(value)
	if idStr == "" {
		errMsg := fmt.Sprintf("%s is required", value)
		return 0, ez.New(op, ez.EINVALID, errMsg, nil)
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, ez.New(op, ez.EINVALID, `Could not parse query to int`, err)
	}

	return id, nil
}

func (h *BaseHandler) GetQueryParamsInt64(c echo.Context, key string) ([]int64, error) {
	const op = "BaseHandler.GetQueryParamsInt64"

	params := c.QueryParams()[key]

	ints := []int64{}
	for _, param := range params {
		id, err := strconv.ParseInt(param, 10, 64)
		if err != nil {
			return nil, ez.New(op, ez.EINVALID, `Could not query params to int`, err)
		}
		ints = append(ints, id)
	}

	return ints, nil
}

func (h *BaseHandler) GetListLimit(c echo.Context, defaultLimit int) int {
	return h.GetNumericQueryParam(c, "limit", defaultLimit)
}

func (h *BaseHandler) GetListOffest(c echo.Context, defaultOffset int) int {
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

	return value
}
