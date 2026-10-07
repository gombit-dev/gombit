package admin

import (
	"context"
	"fmt"
	"reflect"

	"github.com/gombit-dev/gombit/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// relationRead is the read-only metadata for one association admin field: enough
// to read the related primary keys off a preloaded instance, without walking
// arbitrary Go types at request time (#223 / ADR-013). It has no write method —
// has_many uses it directly; many-to-many embeds it and adds the write path.
type relationRead struct {
	name         string // admin field (json) name
	assoc        string // GORM association / struct field name, e.g. "Warehouses"
	fieldIndex   []int  // index of the association slice field on the parent struct
	relatedPKIdx []int  // related model primary key struct-field index
}

// m2mBinding is a relationRead plus the metadata needed to sync a many-to-many
// join table from a submitted id list.
type m2mBinding struct {
	relationRead
	relatedElem   reflect.Type // related struct type (not a pointer)
	relatedPKCol  string       // related model primary key column
	relatedPKType FieldType
}

// findM2M returns the many-to-many binding for an admin field name, matching a
// GORM Many2Many relationship by its json/snake name.
func findM2M(sch *schema.Schema, fieldName string) (*m2mBinding, bool) {
	for _, rel := range sch.Relationships.Many2Many {
		if rel == nil || rel.Field == nil {
			continue
		}
		if relationFieldName(rel.Field) != fieldName {
			continue
		}
		fieldSchema := rel.FieldSchema
		if fieldSchema == nil || len(fieldSchema.PrimaryFields) == 0 {
			return nil, false
		}
		pk := fieldSchema.PrimaryFields[0]
		return &m2mBinding{
			relationRead: relationRead{
				name:         fieldName,
				assoc:        rel.Name,
				fieldIndex:   rel.Field.StructField.Index,
				relatedPKIdx: pk.StructField.Index,
			},
			relatedElem:   fieldSchema.ModelType,
			relatedPKCol:  pk.DBName,
			relatedPKType: inferFieldType(pk),
		}, true
	}
	return nil, false
}

// findHasMany returns a read-only binding for a has_many association: preload
// the slice and extract the related primary keys. It is deliberately a
// relationRead (no write method).
func findHasMany(sch *schema.Schema, fieldName string) (*relationRead, bool) {
	for _, rel := range sch.Relationships.HasMany {
		if rel == nil || rel.Field == nil {
			continue
		}
		if relationFieldName(rel.Field) != fieldName {
			continue
		}
		fieldSchema := rel.FieldSchema
		if fieldSchema == nil || len(fieldSchema.PrimaryFields) == 0 {
			return nil, false
		}
		pk := fieldSchema.PrimaryFields[0]
		return &relationRead{
			name:         fieldName,
			assoc:        rel.Name,
			fieldIndex:   rel.Field.StructField.Index,
			relatedPKIdx: pk.StructField.Index,
		}, true
	}
	return nil, false
}

// relationFieldName is the json/snake name an admin field uses for a GORM
// association struct field.
func relationFieldName(sf *schema.Field) string {
	if name := jsonFieldName(sf); name != "" && name != "-" {
		return name
	}
	return toSnake(sf.Name)
}

// ids reads the related primary keys off a (preloaded) parent instance.
func (b *relationRead) ids(inst any) []any {
	rv := reflect.ValueOf(inst)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return []any{}
		}
		rv = rv.Elem()
	}
	slice := rv.FieldByIndex(b.fieldIndex)
	if slice.Kind() != reflect.Slice {
		return []any{}
	}
	out := make([]any, 0, slice.Len())
	for i := 0; i < slice.Len(); i++ {
		el := slice.Index(i)
		for el.Kind() == reflect.Pointer {
			if el.IsNil() {
				el = reflect.Value{}
				break
			}
			el = el.Elem()
		}
		if !el.IsValid() {
			continue
		}
		out = append(out, el.FieldByIndex(b.relatedPKIdx).Interface())
	}
	return out
}

// sync replaces the join-table rows for inst so the association holds exactly
// the related rows named by ids. Every id must reference an existing related
// row; a missing id is a D10 422 (the target does not exist), never a silent
// no-op.
func (b *m2mBinding) sync(ctx context.Context, db *gorm.DB, inst any, ids []any) error {
	sliceType := reflect.SliceOf(b.relatedElem)
	related := reflect.New(sliceType) // *[]Related
	if len(ids) == 0 {
		// Clear the association.
		if err := db.WithContext(ctx).Model(inst).Association(b.assoc).Clear(); err != nil {
			return contract.WithContext(ctx, contract.Internal("sync relation"))
		}
		return nil
	}
	if err := db.WithContext(ctx).
		Where(b.relatedPKCol+" IN ?", ids).
		Find(related.Interface()).Error; err != nil {
		return contract.WithContext(ctx, contract.Internal("load related rows"))
	}
	found := related.Elem().Len()
	if found != distinctCount(ids) {
		return contract.WithContext(ctx, contract.Validation(
			"The request contains invalid fields.",
			map[string][]string{b.name: {"references a resource that does not exist"}},
		))
	}
	// Replace only manages the join table for already-loaded rows.
	if err := db.WithContext(ctx).Model(inst).Association(b.assoc).Replace(related.Interface()); err != nil {
		return contract.WithContext(ctx, contract.Internal("sync relation"))
	}
	return nil
}

func distinctCount(ids []any) int {
	seen := map[any]struct{}{}
	for _, id := range ids {
		seen[fmt.Sprint(id)] = struct{}{}
	}
	return len(seen)
}

// coerceM2MIDs turns a submitted JSON value (an array of ids, or null) into a
// coerced id slice using the related primary key type.
func coerceM2MIDs(raw any, pkType FieldType) ([]any, error) {
	if raw == nil {
		return []any{}, nil
	}
	rv := reflect.ValueOf(raw)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, fmt.Errorf("must be a list of ids")
	}
	out := make([]any, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		// Ids are compared (WHERE pk IN ?), never stored as given.
		v, err := coerceText(rv.Index(i).Interface(), pkType)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
