package types

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/vanclief/ez"
)

type UnixTimestamp int64

func UnixFromTime(t time.Time) UnixTimestamp {
	return UnixTimestamp(t.Unix())
}

func UnixTimeNow() UnixTimestamp {
	return UnixTimestamp(time.Now().Unix())
}

func UnixFromRFC3339(dateString string) (UnixTimestamp, error) {
	const op = "UnixFromRFC3339"

	date, err := time.Parse(time.RFC3339, dateString)
	if err != nil {
		return 0, ez.Wrap(op, err)
	}

	return UnixTimestamp(date.Unix()), nil
}

func (i UnixTimestamp) Time() time.Time {
	return time.Unix(int64(i), 0).UTC()
}

func (i UnixTimestamp) FormatDate() string {
	return i.Time().Format("02/01/2006")
}

func (i UnixTimestamp) FormatDateInTimezone(tz string) (string, error) {
	const op = "UnixTimestamp.FormatDateInTimezone"

	location, err := time.LoadLocation(tz)
	if err != nil {
		errMsg := fmt.Sprintf("Invalid timezone: %s", tz)
		return "", ez.New(op, ez.EINVALID, errMsg, err)
	}

	return i.Time().In(location).Format("02/01/2006"), nil
}

// UnmarshalJSON accepts either a unix timestamp number or RFC3339 string
func (i *UnixTimestamp) UnmarshalJSON(data []byte) error {
	const op = "UnixTimestamp.UnmarshalJSON"

	// Try to unmarshal as int64 first
	var timestamp int64
	if err := json.Unmarshal(data, &timestamp); err == nil {
		*i = UnixTimestamp(timestamp)
		return nil
	}

	// If that fails, try to unmarshal as string
	var dateStr string
	if err := json.Unmarshal(data, &dateStr); err != nil {
		return ez.New(op, ez.EINVALID, "value must be a unix timestamp or RFC3339 date string", err)
	}

	// Parse the RFC3339 string
	t, err := time.Parse(time.RFC3339, dateStr)
	if err != nil {
		return ez.New(op, ez.EINVALID, "invalid RFC3339 format", err)
	}

	*i = UnixTimestamp(t.Unix())
	return nil
}
