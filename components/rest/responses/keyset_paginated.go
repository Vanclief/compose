package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/vanclief/ez"
)

type KeysetBasedList struct {
	Limit       int    `json:"limit"`
	Hash        string `json:"hash"`
	HasNextPage bool   `json:"has_next_page"`
	NextCursor  string `json:"next_cursor"`
}

func (r *KeysetBasedList) FinalizeResponse(data interface{}, dataLength int) (int, error) {
	const op = "KeysetBasedList.FinalizeResponse"

	responseLength := dataLength

	if responseLength == 0 {
		return responseLength, nil
	}

	// Determine if there is a next page and set the next cursor if applicable
	if dataLength > r.Limit && r.Limit != 0 {
		// We substract the last element as we queried +1 to know if there are more pages
		responseLength = r.Limit
		r.HasNextPage = true
	} else {
		r.HasNextPage = false
		r.NextCursor = ""
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return responseLength, ez.Wrap(op, err)
	}

	hash := sha256.Sum256(jsonData)

	r.Hash = hex.EncodeToString(hash[:])

	return responseLength, nil
}
