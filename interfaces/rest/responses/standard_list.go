package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/vanclief/ez"
)

type StandardList struct {
	Hash       string `json:"hash"`
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
	TotalCount int    `json:"total_count"`
}

func (r *StandardList) GenerateHash(data interface{}) error {
	const op = "StandardList.GenerateHash"

	jsonData, err := json.Marshal(data)
	if err != nil {
		return ez.Wrap(op, err)
	}

	hash := sha256.Sum256(jsonData)

	r.Hash = hex.EncodeToString(hash[:])

	return nil
}
