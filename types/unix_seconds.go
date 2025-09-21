package types

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vanclief/ez"
)

// UnixSeconds is seconds since Unix epoch (1970-01-01T00:00:00Z)
type UnixSeconds int64

// UnixSecondsFromTime converts a time.Time to Unix seconds (truncates sub-second)
func UnixSecondsFromTime(t time.Time) UnixSeconds {
	return UnixSeconds(t.Unix())
}

// UnixSecondsNow returns the current time in Unix seconds
func UnixSecondsNow() UnixSeconds {
	return UnixSeconds(time.Now().Unix())
}

// UnixSecondsFromRFC3339 parses an RFC3339/RFC3339Nano string into Unix seconds
func UnixSecondsFromRFC3339(dateString string) (UnixSeconds, error) {
	const op = "UnixSecondsFromRFC3339"

	date, err := time.Parse(time.RFC3339, dateString)
	if err == nil {
		return UnixSecondsFromTime(date), nil
	}

	date, err = time.Parse(time.RFC3339Nano, dateString)
	if err != nil {
		return 0, ez.New(op, ez.EINVALID, "invalid RFC3339 format", err)
	}

	return UnixSeconds(date.Unix()), nil
}

// Time converts UnixSeconds back to time.Time
func (us UnixSeconds) Time() time.Time {
	return time.Unix(int64(us), 0)
}

// IsZero reports whether us == 0
func (us UnixSeconds) IsZero() bool { return us == 0 }

// FormatDate returns dd/mm/yyyy
func (us UnixSeconds) FormatDate() string {
	return us.Time().Format("02/01/2006")
}

// FormatDateInTimezone returns dd/mm/yyyy rendered in the provided IANA timezone
func (us UnixSeconds) FormatDateInTimezone(tz string) (string, error) {
	const op = "UnixSeconds.FormatDateInTimezone"

	location, err := time.LoadLocation(tz)
	if err != nil {
		errMsg := fmt.Sprintf("invalid timezone: %s", tz)
		return "", ez.New(op, ez.EINVALID, errMsg, err)
	}

	return us.Time().In(location).Format("02/01/2006"), nil
}

// MarshalJSON emits a JSON number (unix seconds)
func (us UnixSeconds) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(us))
}

// UnmarshalJSON accepts a JSON number (unix seconds)
func (us *UnixSeconds) UnmarshalJSON(data []byte) error {
	const op = "UnixSeconds.UnmarshalJSON"

	var seconds int64
	err := json.Unmarshal(data, &seconds)
	if err != nil {
		return ez.New(op, ez.EINVALID, "value must be a JSON number (unix seconds)", err)
	}

	*us = UnixSeconds(seconds)

	return nil
}

// Value implements driver.Valuer for BIGINT storage.
func (us UnixSeconds) Value() (driver.Value, error) { return int64(us), nil }

// Scan implements sql.Scanner for BIGINT (and a few driver variants).
func (us *UnixSeconds) Scan(v any) error {
	switch x := v.(type) {
	case int64:
		*us = UnixSeconds(x)
		return nil
	case int32:
		*us = UnixSeconds(int64(x))
		return nil
	case []byte:
		var n int64
		if _, err := fmt.Sscan(string(x), &n); err != nil {
			return err
		}
		*us = UnixSeconds(n)
		return nil
	case string:
		var n int64
		if _, err := fmt.Sscan(x, &n); err != nil {
			return err
		}
		*us = UnixSeconds(n)
		return nil
	case nil:
		*us = 0
		return nil
	default:
		return fmt.Errorf("unsupported Scan type %T for UnixSeconds", v)
	}
}
