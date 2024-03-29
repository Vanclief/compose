package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

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
	Limit      int `json:"limit"`
	NextCursor int `json:"next_cursor"`
}
