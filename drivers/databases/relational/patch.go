package relational

import (
	"context"
	"reflect"
	"slices"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"
	"github.com/vanclief/compose/types"
	"github.com/vanclief/ez"
)

// Patch collects the columns a partial update writes, so a PATCH request
// changes only the fields the client sent. Writing the whole row instead
// puts back the stale value of every field another request changed since
// the row was loaded.
//
//	var patch relational.Patch
//	relational.SetColumn(&patch, "name", &org.Name, body.Name)
//	relational.SetNullableColumn(&patch, "phone", &org.Phone, body.Phone)
//	org.UpdatedAt = time.Now().UTC()
//	patch.Add("updated_at")
//	err := patch.Update(ctx, db, org)
//
// Patch takes no lock, so it is right for fields that are independent of
// each other. A write whose new value or validity depends on the row's
// current state (a status transition, a counter, a rule across two fields
// that two requests could each change) needs the row locked and re-read in
// the same transaction instead: use Mutate or LockRow.
//
// The zero value is an empty patch.
type Patch struct {
	columns []string
	err     error
}

// Add records a column the caller set on the model itself (updated_at, a
// derived column, or a field taken from a pointer request field). A column
// added twice is recorded once. A caller who wants an empty body to write
// nothing adds its own columns only when len(patch.Columns()) > 0, and still
// calls Update, which returns the error of a null sent for a column that
// cannot hold one.
func (p *Patch) Add(column string) {
	if slices.Contains(p.columns, column) {
		return
	}

	p.columns = append(p.columns, column)
}

// Columns returns the recorded columns in the order they were added.
func (p *Patch) Columns() []string {
	return slices.Clone(p.columns)
}

// SetColumn copies value into dst and records column when value was sent.
// An absent key does nothing. A null is an EINVALID error that Update
// returns, because dst cannot hold null; the error's data names the column.
func SetColumn[T any](p *Patch, column string, dst *T, value types.Optional[T]) {
	if !value.Set {
		return
	}

	if value.Null {
		// The message is shown to the API's user, who sent a JSON key and
		// may never have seen the column's name
		p.fail(ez.New(ez.EINVALID, "A field that cannot be null was sent as null", nil).AddData("column", column))
		return
	}

	*dst = value.Value
	p.Add(column)
}

// SetNullableColumn is SetColumn for a nullable column held as *T: a null
// sets *dst to nil, a value sets it to a pointer to a copy of the value.
// An absent key does nothing.
func SetNullableColumn[T any](p *Patch, column string, dst **T, value types.Optional[T]) {
	if !value.Set {
		return
	}

	if value.Null {
		*dst = nil
		p.Add(column)
		return
	}

	v := value.Value
	*dst = &v
	p.Add(column)
}

// Update validates model and writes only the recorded columns of the row
// model's primary key names. It returns the patch's error first, if a null
// was sent for a column that cannot hold one. With no column recorded it
// returns nil without validating or querying. A recorded primary-key column
// is EINTERNAL and nothing is written: the row is found by the key in model,
// so a changed key would write another row. A row that no longer exists is ENOTFOUND.
func (p *Patch) Update(ctx context.Context, db bun.IDB, model interface{ Validate() error }) error {
	if p.err != nil {
		return ez.Wrap(p.err)
	}

	if len(p.columns) == 0 {
		return nil
	}

	err := model.Validate()
	if err != nil {
		return ez.Wrap(err)
	}

	table, err := rowTable(db, model)
	if err != nil {
		return ez.Wrap(err)
	}

	for _, column := range p.columns {
		// A name bun does not know is left to the UPDATE, which reports it
		field, ok := table.FieldMap[column]
		if ok && field.IsPK {
			return ez.New(ez.EINTERNAL, "The record's identifier cannot be changed", nil).AddData("column", column)
		}
	}

	res, err := db.NewUpdate().Model(model).Column(p.columns...).WherePK().Exec(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return ez.Wrap(err)
	}

	if rows == 0 {
		return ez.New(ez.ENOTFOUND, "The row to update no longer exists", nil)
	}

	return nil
}

// rowTable returns bun's table of model, which must be a non-nil pointer to
// the struct of one row.
func rowTable(db bun.IDB, model any) (*schema.Table, error) {
	value := reflect.ValueOf(model)
	if value.Kind() != reflect.Pointer || value.IsNil() || value.Elem().Kind() != reflect.Struct {
		return nil, ez.New(ez.EINTERNAL, "The record cannot be saved", nil)
	}

	return db.Dialect().Tables().Get(value.Type()), nil
}

// fail keeps the first error recorded on the patch.
func (p *Patch) fail(err error) {
	if p.err == nil {
		p.err = err
	}
}
