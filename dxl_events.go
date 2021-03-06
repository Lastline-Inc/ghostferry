package ghostferry

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/siddontang/go-mysql/replication"
	"github.com/siddontang/go-mysql/schema"
)

type RowData []interface{}

// The mysql driver never actually gives you a uint64 from Scan, instead you
// get an int64 for values that fit in int64 or a byte slice decimal string
// with the uint64 value in it.
// But we have other code in ghostferry that generates DMLEvents that may
// contain unsigned integer types - so we unify the reading of such columns in
// this helper method
func (r RowData) GetInt64(colIdx int) (res int64, err error) {
	rowValue := r[colIdx]
	switch v := rowValue.(type) {
	case uint64:
		res = int64(v)
	case uint32:
		res = int64(v)
	case uint16:
		res = int64(v)
	case uint8:
		res = int64(v)
	case uint:
		res = int64(v)
	case int64:
		res = v
	case int32:
		res = int64(v)
	case int16:
		res = int64(v)
	case int8:
		res = int64(v)
	case int:
		res = int64(v)
	case []byte:
		valueString := string(v)
		res, err = strconv.ParseInt(valueString, 10, 64)
	case string:
		res, err = strconv.ParseInt(v, 10, 64)
	default:
		res = reflect.ValueOf(rowValue).Int()
	}
	return
}

func (r RowData) GetString(colIdx int) string {
	rowValue := r[colIdx]
	switch v := rowValue.(type) {
	case []byte:
		return string(v)
	case string:
		return v
	default:
		return reflect.ValueOf(rowValue).String()
	}
}

// a DXLEvent is the base for DDL or DML
type DXLEvent interface {
	Database() string
	Table() string
	AsSQLString(string, string) (string, error)
	BinlogPosition() BinlogPosition
	EventTime() time.Time
	IsAutoTransaction() bool
}

type DMLEvent interface {
	DXLEvent
	TableSchema() *TableSchema
	OldValues() RowData
	NewValues() RowData
	VerifierPaginationKey() (uint64, error)
}

// The base of DMLEvent/DDLEvent to provide the necessary methods.
type DXLEventBase struct {
	pos  BinlogPosition
	time time.Time
}

func (e *DXLEventBase) BinlogPosition() BinlogPosition {
	return e.pos
}

func (e *DXLEventBase) EventTime() time.Time {
	return e.time
}

func (e *DXLEventBase) IsAutoTransaction() bool {
	return false
}

type DMLEventBase struct {
	table *TableSchema
	*DXLEventBase
}

func (e *DMLEventBase) Database() string {
	return e.table.Schema
}

func (e *DMLEventBase) Table() string {
	return e.table.Name
}

func (e *DMLEventBase) TableSchema() *TableSchema {
	return e.table
}

type BinlogInsertEvent struct {
	newValues RowData
	*DMLEventBase
}

func NewBinlogInsertEvents(table *TableSchema, rowsEvent *replication.RowsEvent, pos BinlogPosition, time time.Time) ([]DMLEvent, error) {
	insertEvents := make([]DMLEvent, len(rowsEvent.Rows))

	for i, row := range rowsEvent.Rows {
		insertEvents[i] = &BinlogInsertEvent{
			newValues:    row,
			DMLEventBase: &DMLEventBase{
				table:        table,
				DXLEventBase: &DXLEventBase{
					pos:  pos,
					time: time,
				},
			},
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

func (e *BinlogInsertEvent) AsSQLString(schemaName, tableName string) (string, error) {
	if err := verifyValuesHasTheSameLengthAsColumns(e.table, e.newValues); err != nil {
		return "", err
	}

	query := "INSERT IGNORE INTO " +
		QuotedTableNameFromString(schemaName, tableName) +
		" (" + strings.Join(quotedColumnNames(e.table), ",") + ")" +
		" VALUES (" + buildStringListForValues(e.table.Columns, e.newValues) + ")"

	return query, nil
}

func (e *BinlogInsertEvent) VerifierPaginationKey() (uint64, error) {
	return verifierPaginationKeyFromEventData(e.table, e.newValues)
}

type BinlogUpdateEvent struct {
	oldValues RowData
	newValues RowData
	*DMLEventBase
}

func NewBinlogUpdateEvents(table *TableSchema, rowsEvent *replication.RowsEvent, pos BinlogPosition, time time.Time) ([]DMLEvent, error) {
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
			DMLEventBase: &DMLEventBase{
				table:        table,
				DXLEventBase: &DXLEventBase{
					pos:  pos,
					time: time,
				},
			},
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

func (e *BinlogUpdateEvent) AsSQLString(schemaName, tableName string) (string, error) {
	if err := verifyValuesHasTheSameLengthAsColumns(e.table, e.oldValues, e.newValues); err != nil {
		return "", err
	}

	query := "UPDATE " + QuotedTableNameFromString(schemaName, tableName) +
		" SET " + buildStringMapForSet(e.table.Columns, e.newValues) +
		" WHERE " + buildStringMapForWhere(e.table.Columns, e.oldValues)

	return query, nil
}

func (e *BinlogUpdateEvent) VerifierPaginationKey() (uint64, error) {
	return verifierPaginationKeyFromEventData(e.table, e.newValues)
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

func NewBinlogDeleteEvents(table *TableSchema, rowsEvent *replication.RowsEvent, pos BinlogPosition, time time.Time) ([]DMLEvent, error) {
	deleteEvents := make([]DMLEvent, len(rowsEvent.Rows))
	for i, row := range rowsEvent.Rows {
		deleteEvents[i] = &BinlogDeleteEvent{
			oldValues:    row,
			DMLEventBase: &DMLEventBase{
				table:        table,
				DXLEventBase: &DXLEventBase{
					pos:  pos,
					time: time,
				},
			},
		}
	}

	return deleteEvents, nil
}

func (e *BinlogDeleteEvent) AsSQLString(schemaName, tableName string) (string, error) {
	if err := verifyValuesHasTheSameLengthAsColumns(e.table, e.oldValues); err != nil {
		return "", err
	}

	query := "DELETE FROM " + QuotedTableNameFromString(schemaName, tableName) +
		" WHERE " + buildStringMapForWhere(e.table.Columns, e.oldValues)

	return query, nil
}

func (e *BinlogDeleteEvent) VerifierPaginationKey() (uint64, error) {
	return verifierPaginationKeyFromEventData(e.table, e.oldValues)
}

func NewBinlogDMLEvents(table *TableSchema, ev *replication.BinlogEvent, pos BinlogPosition, time time.Time) ([]DMLEvent, error) {
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
		return NewBinlogInsertEvents(table, rowsEvent, pos, time)
	case replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2:
		return NewBinlogDeleteEvents(table, rowsEvent, pos, time)
	case replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2:
		return NewBinlogUpdateEvents(table, rowsEvent, pos, time)
	default:
		return nil, fmt.Errorf("unrecognized rows event: %s", ev.Header.EventType.String())
	}
}

type DDLEvent interface {
	DXLEvent
	SqlCommand() string
}

type BinlogSchemaChangeEvent struct {
	sqlCommand    string
	affectedTable *QualifiedTableName
	*DXLEventBase
}

func NewBinlogDDLEvent(command string, affectedTable *QualifiedTableName, pos BinlogPosition, time time.Time) (DDLEvent, error) {
	event := &BinlogSchemaChangeEvent{
		sqlCommand:    command,
		affectedTable: affectedTable,
		DXLEventBase:  &DXLEventBase{
			pos:  pos,
			time: time,
		},
	}
	return event, nil
}

func (e *BinlogSchemaChangeEvent) IsAutoTransaction() bool {
	// a schema change will always build its own transaction and commit an ongoing one!
	return true
}

func (e *BinlogSchemaChangeEvent) Database() string {
	return e.affectedTable.SchemaName
}

func (e *BinlogSchemaChangeEvent) Table() string {
	return e.affectedTable.TableName
}

func (e *BinlogSchemaChangeEvent) SqlCommand() string {
	return e.sqlCommand
}

func (e *BinlogSchemaChangeEvent) AsSQLString(schemaName, tableName string) (string, error) {
	// We don't support altering tables schemas, dropping/adding tables, etc
	// when remapping table names. We would have to do deeply-nested rewrites
	// of a statement to enforce constraints
	//
	// The same is true for re-mapping the name of the database, because the
	// migration command may specify the name of the target database, and our
	// "USE <db>" (see below) becomes useless
	if schemaName != e.affectedTable.SchemaName || tableName != e.affectedTable.TableName {
		return "", fmt.Errorf(
			"cannot use remapped schema/tableName %s.%s for migration of %s.%s",
			schemaName,
			tableName,
			e.affectedTable.SchemaName,
			e.affectedTable.TableName,
		)
	}

	// In case the command changes the schema of a table without specifying the
	// name of the DB (which is actually the most common way to write things
	// like ALTER TABLE), we need to specify the active DB on which to apply the
	// migration.
	query := "USE " + QuotedDatabaseNameFromString(schemaName) + ";\n" + e.sqlCommand
	return query, nil
}

func quotedColumnNames(table *TableSchema) []string {
	cols := make([]string, len(table.Columns))
	for i, column := range table.Columns {
		cols[i] = quoteField(column.Name)
	}

	return cols
}

func verifyValuesHasTheSameLengthAsColumns(table *TableSchema, values ...RowData) error {
	for _, v := range values {
		if len(table.Columns) != len(v) {
			return fmt.Errorf(
				"table %s.%s has %d columns but event has %d columns instead",
				table.Schema,
				table.Name,
				len(table.Columns),
				len(v),
			)
		}
	}
	return nil
}

func buildStringListForValues(columns []schema.TableColumn, values []interface{}) string {
	var buffer []byte

	for i, value := range values {
		if i > 0 {
			buffer = append(buffer, ',')
		}

		buffer = appendEscapedValue(buffer, value, columns[i])
	}

	return string(buffer)
}

func buildStringMapForWhere(columns []schema.TableColumn, values []interface{}) string {
	var buffer []byte

	for i, value := range values {
		if i > 0 {
			buffer = append(buffer, " AND "...)
		}

		buffer = append(buffer, quoteField(columns[i].Name)...)

		if isNilValue(value) {
			// "WHERE value = NULL" will never match rows.
			buffer = append(buffer, " IS NULL"...)
		} else {
			buffer = append(buffer, '=')
			buffer = appendEscapedValue(buffer, value, columns[i])
		}
	}

	return string(buffer)
}

func buildStringMapForSet(columns []schema.TableColumn, values []interface{}) string {
	var buffer []byte

	for i, value := range values {
		if i > 0 {
			buffer = append(buffer, ',')
		}

		buffer = append(buffer, quoteField(columns[i].Name)...)
		buffer = append(buffer, '=')
		buffer = appendEscapedValue(buffer, value, columns[i])
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

func appendEscapedValue(buffer []byte, value interface{}, column schema.TableColumn) []byte {
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
		var rightPadLengthForBinaryColumn int
		// see appendEscapedString() for details why we need special
		// handling of BINARY column types
		if column.Type == schema.TYPE_BINARY {
			rightPadLengthForBinaryColumn = int(column.FixedSize)
		}
		return appendEscapedString(buffer, v, rightPadLengthForBinaryColumn)
	case []byte:
		return appendEscapedBuffer(buffer, v, column.Type == schema.TYPE_JSON)
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
		return appendEscapedString(buffer, v.String(), 0)
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
//
// We need to support right-padding of the generated string using 0-bytes to
// mimic what a MySQL server would do for BINARY columns (with fixed length).
//
// ref: https://github.com/Shopify/ghostferry/pull/159
//
// This is specifically mentioned in the the below link:
//
//    When BINARY values are stored, they are right-padded with the pad value
//    to the specified length. The pad value is 0x00 (the zero byte). Values
//    are right-padded with 0x00 for inserts, and no trailing bytes are removed
//    for retrievals.
//
// ref: https://dev.mysql.com/doc/refman/5.7/en/binary-varbinary.html
func appendEscapedString(buffer []byte, value string, rightPadToLengthWithZeroBytes int) []byte {
	buffer = append(buffer, '\'')

	var i int
	for i = 0; i < len(value); i++ {
		c := value[i]
		if c == '\'' {
			buffer = append(buffer, '\'', '\'')
		} else {
			buffer = append(buffer, c)
		}
	}
	// continue 0-padding up to the desired length as provided by the
	// caller
	if i < rightPadToLengthWithZeroBytes {
		buffer = rightPadBufferWithZeroBytes(buffer, rightPadToLengthWithZeroBytes-i)
	}

	return append(buffer, '\'')
}

func rightPadBufferWithZeroBytes(buffer []byte, padLength int) []byte {
	for i := 0; i < padLength; i++ {
		buffer = append(buffer, '\x00')
	}
	return buffer
}

func appendEscapedBuffer(buffer, value []byte, isJSON bool) []byte {
	if isJSON {
		// See https://bugs.mysql.com/bug.php?id=98496
		if len(value) == 0 {
			value = []byte("null")
		}

		buffer = append(buffer, "CAST("...)
	} else {
		buffer = append(buffer, "_binary"...)
	}

	buffer = append(buffer, '\'')

	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '\'' {
			buffer = append(buffer, '\'', '\'')
		} else {
			buffer = append(buffer, c)
		}
	}

	buffer = append(buffer, '\'')

	if isJSON {
		buffer = append(buffer, " AS JSON)"...)
	}

	return buffer
}

func verifierPaginationKeyFromEventData(table *TableSchema, rowData RowData) (uint64, error) {
	if err := verifyValuesHasTheSameLengthAsColumns(table, rowData); err != nil {
		return 0, err
	}

	if table.PaginationKey == nil {
		return 0, fmt.Errorf("table %s does not have a pagination key", table)
	}

	if !table.PaginationKey.IsLinearUnsignedKey() {
		return 0, UnsupportedPaginationKeyError(table.Schema, table.Name, table.PaginationKey.String())
	}

	value, err := rowData.GetInt64(table.PaginationKey.ColumnIndices[0])
	if err != nil {
		return 0, err
	}

	// for legacy-compatibility, we allow signed pagination keys, so we have to
	// make sure no signed data snuck in
	if value < 0 {
		return 0, fmt.Errorf("table %s contains an unsupported (signed) pagination key value %d", table, value)
	}

	return uint64(value), nil
}
