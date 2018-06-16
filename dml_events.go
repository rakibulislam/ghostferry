package ghostferry

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/siddontang/go-mysql/replication"
	"github.com/siddontang/go-mysql/schema"
)

type RowData []interface{}

// The mysql driver never actually gives you a uint64 from Scan, instead you
// get an int64 for values that fit in int64 or a byte slice decimal string
// with the uint64 value in it.
func (r RowData) GetUint64(colIdx int) (res uint64, err error) {
	if valueByteSlice, ok := r[colIdx].([]byte); ok {
		valueString := string(valueByteSlice)
		res, err = strconv.ParseUint(valueString, 10, 64)
		if err != nil {
			return 0, err
		}
	} else {
		signedInt := reflect.ValueOf(r[colIdx]).Int()
		if signedInt < 0 {
			return 0, fmt.Errorf("expected position %d in row to contain an unsigned number", colIdx)
		}
		res = uint64(signedInt)
	}
	return
}

type DMLEvent interface {
	Database() string
	Table() string
	TableSchema() *schema.Table
	AsSQLString(intersection *schema.Table) (string, error)
	OldValues() RowData
	NewValues() RowData
	PK() (uint64, error)
}

// The base of DMLEvent to provide the necessary methods.
// This desires a copy of the struct in case we want to deal with schema
// changes in the future.
type DMLEventBase struct {
	table schema.Table
}

func (e *DMLEventBase) Database() string {
	return e.table.Schema
}

func (e *DMLEventBase) Table() string {
	return e.table.Name
}

func (e *DMLEventBase) TableSchema() *schema.Table {
	return &e.table
}

func (e *DMLEventBase) pkFromEventData(rowData RowData) (uint64, error) {
	if err := verifyValuesHasTheSameLengthAsColumns(&e.table, rowData); err != nil {
		return 0, err
	}

	pkIndex := e.table.PKColumns[0]
	return rowData.GetUint64(pkIndex)
}

type BinlogInsertEvent struct {
	newValues RowData
	*DMLEventBase
}

func NewBinlogInsertEvents(table *schema.Table, rowsEvent *replication.RowsEvent) ([]DMLEvent, error) {
	insertEvents := make([]DMLEvent, len(rowsEvent.Rows))

	for i, row := range rowsEvent.Rows {
		insertEvents[i] = &BinlogInsertEvent{
			newValues:    row,
			DMLEventBase: &DMLEventBase{table: *table},
		}
	}

	return insertEvents, nil
}

func (e *BinlogInsertEvent) OldValues() RowData {
	return nil
}

func (e *BinlogInsertEvent) NewValues() RowData {
	return e.newValues
}

func (e *BinlogInsertEvent) AsSQLString(intersection *schema.Table) (string, error) {
	columns, values, err := loadColumnsAndValuesInIntersection(&e.table, intersection, e.newValues)
	if err != nil {
		return "", err
	}

	query := "INSERT IGNORE INTO " +
		QuotedTableNameFromString(intersection.Schema, intersection.Name) +
		" (" + strings.Join(columns, ",") + ")" +
		" VALUES (" + buildStringListForValues(values[0]) + ")"

	return query, nil
}

func (e *BinlogInsertEvent) PK() (uint64, error) {
	return e.pkFromEventData(e.newValues)
}

type BinlogUpdateEvent struct {
	oldValues RowData
	newValues RowData
	*DMLEventBase
}

func NewBinlogUpdateEvents(table *schema.Table, rowsEvent *replication.RowsEvent) ([]DMLEvent, error) {
	// UPDATE events have two rows in the RowsEvent. The first row is the
	// entries of the old record (for WHERE) and the second row is the
	// entries of the new record (for SET).
	// There can be n db rows changed in one RowsEvent, resulting in
	// 2*n binlog rows.
	updateEvents := make([]DMLEvent, len(rowsEvent.Rows)/2)

	for i, row := range rowsEvent.Rows {
		if i%2 == 1 {
			continue
		}

		updateEvents[i/2] = &BinlogUpdateEvent{
			oldValues:    row,
			newValues:    rowsEvent.Rows[i+1],
			DMLEventBase: &DMLEventBase{table: *table},
		}
	}

	return updateEvents, nil
}

func (e *BinlogUpdateEvent) OldValues() RowData {
	return e.oldValues
}

func (e *BinlogUpdateEvent) NewValues() RowData {
	return e.newValues
}

func (e *BinlogUpdateEvent) AsSQLString(intersection *schema.Table) (string, error) {
	columns, values, err := loadColumnsAndValuesInIntersection(&e.table, intersection, e.oldValues, e.newValues)
	if err != nil {
		return "", err
	}

	query := "UPDATE " + QuotedTableNameFromString(intersection.Schema, intersection.Name) +
		" SET " + buildStringMapForSet(columns, values[1]) +
		" WHERE " + buildStringMapForWhere(columns, values[0])

	return query, nil
}

func (e *BinlogUpdateEvent) PK() (uint64, error) {
	return e.pkFromEventData(e.newValues)
}

type BinlogDeleteEvent struct {
	oldValues RowData
	*DMLEventBase
}

func (e *BinlogDeleteEvent) OldValues() RowData {
	return e.oldValues
}

func (e *BinlogDeleteEvent) NewValues() RowData {
	return nil
}

func NewBinlogDeleteEvents(table *schema.Table, rowsEvent *replication.RowsEvent) ([]DMLEvent, error) {
	deleteEvents := make([]DMLEvent, len(rowsEvent.Rows))

	for i, row := range rowsEvent.Rows {
		deleteEvents[i] = &BinlogDeleteEvent{
			oldValues:    row,
			DMLEventBase: &DMLEventBase{table: *table},
		}
	}

	return deleteEvents, nil
}

func (e *BinlogDeleteEvent) AsSQLString(intersection *schema.Table) (string, error) {
	columns, values, err := loadColumnsAndValuesInIntersection(&e.table, intersection, e.oldValues)
	if err != nil {
		return "", err
	}

	query := "DELETE FROM " + QuotedTableNameFromString(intersection.Schema, intersection.Name) +
		" WHERE " + buildStringMapForWhere(columns, values[0])

	return query, nil
}

func (e *BinlogDeleteEvent) PK() (uint64, error) {
	return e.pkFromEventData(e.oldValues)
}

func NewBinlogDMLEvents(table *schema.Table, ev *replication.BinlogEvent) ([]DMLEvent, error) {
	rowsEvent := ev.Event.(*replication.RowsEvent)

	for _, row := range rowsEvent.Rows {
		if len(row) != len(table.Columns) {
			return nil, fmt.Errorf(
				"table %s.%s has %d columns but event has %d columns instead",
				table.Schema,
				table.Name,
				len(table.Columns),
				len(row),
			)
		}
		for i, col := range table.Columns {
			if col.IsUnsigned {
				switch v := row[i].(type) {
				case int64:
					row[i] = uint64(v)
				case int32:
					row[i] = uint32(v)
				case int16:
					row[i] = uint16(v)
				case int8:
					row[i] = uint8(v)
				case int:
					row[i] = uint(v)
				}
			}
		}
	}

	switch ev.Header.EventType {
	case replication.WRITE_ROWS_EVENTv1, replication.WRITE_ROWS_EVENTv2:
		return NewBinlogInsertEvents(table, rowsEvent)
	case replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2:
		return NewBinlogDeleteEvents(table, rowsEvent)
	case replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2:
		return NewBinlogUpdateEvents(table, rowsEvent)
	default:
		return nil, fmt.Errorf("unrecognized rows event: %s", ev.Header.EventType.String())
	}
}

func loadColumnsAndValuesInIntersection(table *schema.Table, intersection *schema.Table, valuesToVerify ...RowData) ([]string, []RowData, error) {
	var intersectedColumns []int
	var quotedNames []string

	if len(table.Columns) == 0 || len(intersection.Columns) == 0 {
		panic(fmt.Sprintf("zero columns: table: %d, intersection: %d", len(table.Columns), len(intersection.Columns)))
	}

	for colIdx, col := range table.Columns {
		for _, targetCol := range intersection.Columns {
			if col.Name == targetCol.Name {
				intersectedColumns = append(intersectedColumns, colIdx)
				quotedNames = append(quotedNames, quoteField(col.Name))
				break
			}
		}
	}

	var intersectedValueList []RowData

	for _, values := range valuesToVerify {
		if err := verifyValuesHasTheSameLengthAsColumns(table, values); err != nil {
			return nil, nil, err
		}

		var intersectedValues RowData

		for _, colIdx := range intersectedColumns {
			intersectedValues = append(intersectedValues, values[colIdx])
		}

		intersectedValueList = append(intersectedValueList, intersectedValues)
	}

	return quotedNames, intersectedValueList, nil
}

func verifyValuesHasTheSameLengthAsColumns(table *schema.Table, values RowData) error {
	if len(table.Columns) != len(values) {
		return fmt.Errorf(
			"table %s.%s has %d columns but event has %d columns instead",
			table.Schema,
			table.Name,
			len(table.Columns),
			len(values),
		)
	}
	return nil
}

func buildStringListForValues(values []interface{}) string {
	var buffer []byte

	for i, value := range values {
		if i > 0 {
			buffer = append(buffer, ',')
		}

		buffer = appendEscapedValue(buffer, value)
	}

	return string(buffer)
}

func buildStringMapForWhere(columns []string, values []interface{}) string {
	var buffer []byte

	for i, value := range values {
		if i > 0 {
			buffer = append(buffer, " AND "...)
		}

		buffer = append(buffer, columns[i]...)

		if isNilValue(value) {
			// "WHERE value = NULL" will never match rows.
			buffer = append(buffer, " IS NULL"...)
		} else {
			buffer = append(buffer, '=')
			buffer = appendEscapedValue(buffer, value)
		}
	}

	return string(buffer)
}

func buildStringMapForSet(columns []string, values []interface{}) string {
	var buffer []byte

	for i, value := range values {
		if i > 0 {
			buffer = append(buffer, ',')
		}

		buffer = append(buffer, columns[i]...)
		buffer = append(buffer, '=')
		buffer = appendEscapedValue(buffer, value)
	}

	return string(buffer)
}

func isNilValue(value interface{}) bool {
	if value == nil {
		return true
	} else if vb, ok := value.([]byte); ok && vb == nil {
		return true
	}
	return false
}

func appendEscapedValue(buffer []byte, value interface{}) []byte {
	if isNilValue(value) {
		return append(buffer, "NULL"...)
	}

	if uintv, ok := Uint64Value(value); ok {
		return strconv.AppendUint(buffer, uintv, 10)
	}

	if intv, ok := Int64Value(value); ok {
		return strconv.AppendInt(buffer, intv, 10)
	}

	switch v := value.(type) {
	case string:
		return appendEscapedString(buffer, v)
	case []byte:
		return appendEscapedBuffer(buffer, v)
	case bool:
		if v {
			return append(buffer, '1')
		} else {
			return append(buffer, '0')
		}
	case float64:
		return strconv.AppendFloat(buffer, v, 'g', -1, 64)
	case float32:
		return strconv.AppendFloat(buffer, float64(v), 'g', -1, 64)
	case decimal.Decimal:
		return appendEscapedString(buffer, v.String())
	default:
		panic(fmt.Sprintf("unsupported type %t", value))
	}
}

func Uint64Value(value interface{}) (uint64, bool) {
	switch v := value.(type) {
	case uint64:
		return v, true
	case uint32:
		return uint64(v), true
	case uint16:
		return uint64(v), true
	case uint8:
		return uint64(v), true
	case uint:
		return uint64(v), true
	}
	return 0, false
}

func Int64Value(value interface{}) (int64, bool) {
	switch v := value.(type) {
	case int64:
		return v, true
	case int32:
		return int64(v), true
	case int16:
		return int64(v), true
	case int8:
		return int64(v), true
	case int:
		return int64(v), true
	}
	return 0, false
}

// appendEscapedString replaces single quotes with quote-escaped single quotes.
// When the NO_BACKSLASH_ESCAPES mode is on, this is the extent of escaping
// necessary for strings.
//
// ref: https://github.com/mysql/mysql-server/blob/mysql-5.7.5/mysys/charset.c#L963-L1038
// ref: https://github.com/go-sql-driver/mysql/blob/9181e3a86a19bacd63e68d43ae8b7b36320d8092/utils.go#L717-L758
func appendEscapedString(buffer []byte, value string) []byte {
	buffer = append(buffer, '\'')

	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '\'' {
			buffer = append(buffer, '\'', '\'')
		} else {
			buffer = append(buffer, c)
		}
	}

	return append(buffer, '\'')
}

func appendEscapedBuffer(buffer, value []byte) []byte {
	buffer = append(buffer, "_binary'"...)

	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '\'' {
			buffer = append(buffer, '\'', '\'')
		} else {
			buffer = append(buffer, c)
		}
	}

	return append(buffer, '\'')
}
