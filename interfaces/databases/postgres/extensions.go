package postgres

import (
	"context"
	"fmt"

	"github.com/vanclief/ez"
)

// CreateExtensions - Creates a database extension if it doesn't already exist
func (db *DB) CreateExtensions(extensions []string) error {
	const op = "database.CreateExtensions"

	ctx := context.Background()

	for _, extension := range extensions {
		rawQuery := fmt.Sprintf(`CREATE EXTENSION IF NOT EXISTS "%s";`, extension)
		_, err := db.NewRaw(rawQuery).Exec(ctx)
		if err != nil {
			return ez.Wrap(op, err)
		}
	}

	return nil
}
