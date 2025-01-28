package pagination

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
	"github.com/vanclief/compose/interfaces/databases/relational/query"
	"github.com/vanclief/ez"
)

const (
	MAX_PAGE_SIZE = 1000
	DEFAULT_LIMIT = 50
)

// Cursor holds the values needed for pagination
type Cursor struct {
	SortField   string      `json:"sort_field"`
	SortValue   interface{} `json:"sort_value"`
	UniqueField string      `json:"unique_field"`
	UniqueValue interface{} `json:"unique_value"`
}

// EncodeCursor creates a base64 encoded cursor from a Cursor struct
func EncodeCursor(c Cursor) (string, error) {
	const op = "pagination.EncodeCursor"

	jsonBytes, err := json.Marshal(c)
	if err != nil {
		return "", ez.New(op, ez.EINTERNAL, "failed to marshal cursor", err)
	}

	return base64.URLEncoding.EncodeToString(jsonBytes), nil
}

// DecodeCursor decodes a base64 encoded cursor string into a Cursor struct
func DecodeCursor(encodedCursor string) (*Cursor, error) {
	const op = "pagination.DecodeCursor"

	if encodedCursor == "" {
		return nil, nil
	}

	jsonBytes, err := base64.URLEncoding.DecodeString(encodedCursor)
	if err != nil {
		return nil, ez.New(op, ez.EINVALID, "invalid cursor format", err)
	}

	var cursor Cursor
	err = json.Unmarshal(jsonBytes, &cursor)
	if err != nil {
		return nil, ez.New(op, ez.EINVALID, "invalid cursor data", err)
	}

	return &cursor, nil
}

// CursorRequest represents a cursor paginated request
type CursorRequest struct {
	Limit  int            `json:"limit"`
	Cursor string         `json:"cursor"`
	Filter []query.Filter `json:"filters,omitempty"`
}

func (r *CursorRequest) Validate() error {
	if r.Limit > MAX_PAGE_SIZE {
		return ez.New("CursorRequest.Validate", ez.EINVALID, "Page size must be less than 1000", nil)
	} else if r.Limit == 0 {
		r.Limit = DEFAULT_LIMIT
	}

	return nil
}

// ApplyCursorToQuery adds cursor-based pagination to a bun query
func ApplyCursorToQuery(query *bun.SelectQuery, r *CursorRequest, sortField, uniqueField string) (*bun.SelectQuery, error) {
	const op = "pagination.ApplyCursorToQuery"

	if r.Limit <= 0 {
		return nil, ez.New(op, ez.EINVALID, "limit must be greater than 0", nil)
	}

	// Add 1 to limit to check if there's a next page
	query = query.Limit(r.Limit + 1)

	cursor, err := DecodeCursor(r.Cursor)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	if cursor != nil {
		condition := strings.Builder{}
		condition.WriteString(fmt.Sprintf("(%s < ? OR (%s = ? AND %s < ?))",
			sortField, sortField, uniqueField))

		query = query.Where(condition.String(),
			cursor.SortValue, cursor.SortValue, cursor.UniqueValue)
	}

	// Apply all filters
	for _, filter := range r.Filter {
		query = query.Where(fmt.Sprintf("%s %s ?", filter.Field, filter.Comparison), filter.Value)
	}

	return query, nil
}

// Paginatable interface that models must implement to use pagination
type Paginatable interface {
	GetSortField() string
	GetSortValue() interface{}
	GetUniqueField() string
	GetUniqueValue() interface{}
}

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
