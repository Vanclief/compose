package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/vanclief/compose/types"
	"github.com/vanclief/ez"
)

type OffsetBasedList struct {
	Hash       string `json:"hash"`
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
	TotalCount int    `json:"total_count"`
}

func (r *OffsetBasedList) GenerateHash(data interface{}) error {
	const op = "OffsetBasedList.GenerateHash"

	jsonData, err := json.Marshal(data)
	if err != nil {
		return ez.Wrap(op, err)
	}

	hash := sha256.Sum256(jsonData)

	r.Hash = hex.EncodeToString(hash[:])

	return nil
}

type KeysetBasedList struct {
	Limit       int    `json:"limit"`
	Hash        string `json:"hash"`
	HasNextPage bool   `json:"has_next_page"`
	NextCursor  string `json:"next_cursor"`
}

func (r *KeysetBasedList) FinalizeResponse(data []types.ModelWithCursor) ([]types.ModelWithCursor, error) {
	const op = "KeysetBasedList.FinalizeResponse"

	if len(data) == 0 {
		return data, nil
	}

	// Determine if there is a next page and set the next cursor if applicable
	if len(data) > r.Limit && r.Limit != 0 {
		// We substract the last element as we queried +1 to know if there are more pages
		data = data[:r.Limit-1]
		r.HasNextPage = true
		r.NextCursor = data[len(data)-1].GetCursor()
	} else {
		r.HasNextPage = false
		r.NextCursor = ""
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	hash := sha256.Sum256(jsonData)

	r.Hash = hex.EncodeToString(hash[:])

	return data, nil
}
