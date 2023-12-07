package handler

import (
	"fmt"
	"strconv"

	"github.com/labstack/echo/v4"
	"github.com/vanclief/ez"
)

func GetParameterID(c echo.Context, name string) (int64, error) {
	const op = "getParameterID"

	idStr, err := GetParameterString(c, name)
	if err != nil {
		return 0, ez.Wrap(op, err)
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, ez.Wrap(op, err)
	}

	return id, nil
}

func GetParameterString(c echo.Context, name string) (string, error) {
	const op = "getParameterString"

	idStr := c.Param(name)
	if idStr == "" {
		return "", ez.New(op, ez.EINVALID, `Resource ID is required`, nil)
	}

	return idStr, nil
}

func GetQueryID(c echo.Context, value string) (int64, error) {
	const op = "getQueryID"

	idStr := c.QueryParam(value)
	if idStr == "" {
		errMsg := fmt.Sprintf("%s is required", value)
		return 0, ez.New(op, ez.EINVALID, errMsg, nil)
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, ez.Wrap(op, err)
	}

	return id, nil
}

func GetQueryParamsInt64(c echo.Context, key string) ([]int64, error) {
	const op = "getQueryParamsInt64"

	params := c.QueryParams()[key]

	ints := []int64{}
	for _, param := range params {
		id, err := strconv.ParseInt(param, 10, 64)
		if err != nil {
			return nil, ez.Wrap(op, err)
		}
		ints = append(ints, id)
	}

	return ints, nil
}

func GetListLimit(c echo.Context, defaultLimit int) int {
	return GetNumericQueryParam(c, "limit", defaultLimit)
}

func GetListOffest(c echo.Context, defaultOffset int) int {
	return GetNumericQueryParam(c, "offset", defaultOffset)
}

func GetNumericQueryParam(c echo.Context, param string, defaultValue int) int {
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
