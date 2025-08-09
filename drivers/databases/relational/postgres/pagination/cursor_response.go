package pagination

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/vanclief/ez"
)

// CursorResponse represents a cursor paginated response
type CursorResponse struct {
	items       interface{} `json:"-"` // Private field to hold items temporarily
	NextCursor  string      `json:"next_cursor,omitempty"`
	HasNextPage bool        `json:"has_next_page"`
	Hash        string      `json:"hash,omitempty"` // SHA-256 hash of the items for data freshness validation
}

// GetItems returns the underlying items of the response
func (r *CursorResponse) GetItems() interface{} {
	return r.items
}

// BuildCursorResponse processes the query results and creates a paginated response
func BuildCursorResponse[T Paginatable](items []T, limit int) (*CursorResponse, error) {
	const op = "pagination.BuildCursorResponse"

	hasNextPage := len(items) > limit
	if hasNextPage {
		items = items[:limit]
	}

	var nextCursor string
	if hasNextPage && len(items) > 0 {
		lastItem := items[len(items)-1]
		cursor := Cursor{
			SortField:   lastItem.GetSortField(),
			SortValue:   lastItem.GetSortValue(),
			UniqueField: lastItem.GetUniqueField(),
			UniqueValue: lastItem.GetUniqueValue(),
		}

		var err error
		nextCursor, err = EncodeCursor(cursor)
		if err != nil {
			return nil, ez.Wrap(op, err)
		}
	}

	// Calculate hash from items
	jsonData, err := json.Marshal(items)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	h := sha256.Sum256(jsonData)
	hash := hex.EncodeToString(h[:])

	return &CursorResponse{
		items:       items,
		NextCursor:  nextCursor,
		HasNextPage: hasNextPage,
		Hash:        hash,
	}, nil
}
