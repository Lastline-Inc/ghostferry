package ghostferry

import (
	sqlorig "database/sql"
	"fmt"
	sql "github.com/Shopify/ghostferry/sqlwrapper"
	"strings"

	sq "github.com/Masterminds/squirrel"
	"github.com/siddontang/go-mysql/schema"
	"github.com/sirupsen/logrus"
)

var ignoredDatabases = map[string]bool{
	"mysql":              true,
	"information_schema": true,
	"performance_schema": true,
	"sys":                true,
}

// A comparable and lightweight type that stores the schema and table name.
type TableIdentifier struct {
	SchemaName string
	TableName  string
}

func NewTableIdentifierFromSchemaTable(table *TableSchema) TableIdentifier {
	return TableIdentifier{
		SchemaName: table.Schema,
		TableName:  table.Name,
	}
}

type PaginationKey struct {
	// The sorted list of columns
	Columns []*schema.TableColumn

	// The sorted indices of the columns as they appear in the table
	ColumnIndices []int

	// Optional index of the column that we consider most indicative of
	// progress. Such an entry may not exist and can be set to -1 to explicitly
	// indicating that it does not exist.
	// Used only for estimating the position of a particular value within the
	// range of the pagination
	MostSignificantColumnIndex int
}

func (k PaginationKey) String() string {
	s := "PaginationKey("
	for i, column := range k.Columns {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%s[%d]", column.Name, k.ColumnIndices[i])
	}
	return s + ")"
}

func (k PaginationKey) IsLinearUnsignedKey() bool {
	// The upstream ghostferry code assumes all pagination keys are unsigned
	// single-column primary keys. This requirement has been lifted from most
	// of the code, including copy and streaming, but not from the verifiers
	// yet.
	// This property validates if those functionalities are compatible with
	// this pagination key
	if len(k.Columns) == 1 {
		column := k.Columns[0]
		// NOTE: Technically, we should also enforce column.IsUnsigned here,
		// but it seems that requirement was always in ghostferry but never
		// explicitly enforces - enforcing it now would break backwards
		// compatibility, so we don't.
		return column.Type == schema.TYPE_NUMBER
	}
	return false
}

// This is a wrapper on schema.Table with some custom information we need.
type TableSchema struct {
	*schema.Table

	CompressedColumnsForVerification map[string]string   // Map of column name => compression type
	IgnoredColumnsForVerification    map[string]struct{} // Set of column name
	PaginationKey                    *PaginationKey

	rowMd5Query string
}

// This query returns the MD5 hash for a row on this table. This query is valid
// for both the source and the target shard.
//
// Any compressed columns specified via CompressedColumnsForVerification are
// excluded in this checksum and the raw data is returned directly.
//
// Any columns specified in IgnoredColumnsForVerification are excluded from the
// checksum and the raw data will not be returned.
//
// Note that the MD5 hash should consists of at least 1 column: the paginationKey column.
// This is to say that there should never be a case where the MD5 hash is
// derived from an empty string.
func (t *TableSchema) FingerprintQuery(schemaName, tableName string, numRows int) (string, error) {
	if !t.PaginationKey.IsLinearUnsignedKey() {
		return "", UnsupportedPaginationKeyError(t.Schema, t.Name, t.PaginationKey.String())
	}

	columnsToSelect := make([]string, 2+len(t.CompressedColumnsForVerification))
	columnsToSelect[0] = quoteField(t.PaginationKey.Columns[0].Name)
	columnsToSelect[1] = t.RowMd5Query()
	i := 2
	for columnName, _ := range t.CompressedColumnsForVerification {
		columnsToSelect[i] = quoteField(columnName)
		i += 1
	}

	return fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s IN (%s)",
		strings.Join(columnsToSelect, ","),
		QuotedTableNameFromString(schemaName, tableName),
		columnsToSelect[0],
		strings.Repeat("?,", numRows-1)+"?",
	), nil
}

func (t *TableSchema) RowMd5Query() string {
	if t.rowMd5Query != "" {
		return t.rowMd5Query
	}

	columns := make([]schema.TableColumn, 0, len(t.Columns))
	for _, column := range t.Columns {
		_, isCompressed := t.CompressedColumnsForVerification[column.Name]
		_, isIgnored := t.IgnoredColumnsForVerification[column.Name]

		if isCompressed || isIgnored {
			continue
		}

		columns = append(columns, column)
	}

	hashStrs := make([]string, len(columns))
	for i, column := range columns {
		// Magic string that's unlikely to be a real record. For a history of this
		// issue, refer to https://github.com/Shopify/ghostferry/pull/137
		hashStrs[i] = fmt.Sprintf("MD5(COALESCE(%s, 'NULL_PBj}b]74P@JTo$5G_null'))", normalizeAndQuoteColumn(column))
	}

	t.rowMd5Query = fmt.Sprintf("MD5(CONCAT(%s)) AS __ghostferry_row_md5", strings.Join(hashStrs, ","))
	return t.rowMd5Query
}

type TableSchemaCache map[string]*TableSchema

func fullTableName(schemaName, tableName string) string {
	return fmt.Sprintf("%s.%s", schemaName, tableName)
}

func QuotedDatabaseNameFromString(database string) string {
	return fmt.Sprintf("`%s`", database)
}

func QuotedTableName(table *TableSchema) string {
	return QuotedTableNameFromString(table.Schema, table.Name)
}

func QuotedTableNameFromString(database, table string) string {
	return fmt.Sprintf("`%s`.`%s`", database, table)
}

func GetTargetPaginationKeys(db *sql.DB, tables []*TableSchema, iterateInDescendingOrder bool, logger *logrus.Entry) (paginatedTables map[*TableSchema]*PaginationKeyData, unpaginatedTables []*TableSchema, err error) {
	paginatedTables = make(map[*TableSchema]*PaginationKeyData)
	unpaginatedTables = make([]*TableSchema, 0, len(tables))

	for _, table := range tables {
		logger := logger.WithField("table", table.String())

		isEmpty, dbErr := isEmptyTable(db, table)
		if dbErr != nil {
			err = dbErr
			return
		}
		// NOTE: We treat empty tables just like any other non-paginated table
		// to make sure they are marked as completed in the state-tracker (if
		// they are not already)
		if isEmpty || table.PaginationKey == nil {
			logger.Debugf("tracking as unpaginated table (empty: %v)", isEmpty)
			unpaginatedTables = append(unpaginatedTables, table)
			continue
		}

		targetPaginationKey, targetPaginationKeyExists, paginationErr := targetPaginationKey(db, table, iterateInDescendingOrder)
		if paginationErr != nil {
			logger.WithError(paginationErr).Errorf("failed to get target primary key %s", table.PaginationKey)
			err = paginationErr
			return
		}

		if !targetPaginationKeyExists {
			// potential race in the setup
			logger.Debugf("tracking as unpaginated table (no pagination key)")
			unpaginatedTables = append(unpaginatedTables, table)
			continue
		}

		logger.Debugf("tracking as paginated table with target-pagination %s", targetPaginationKey)
		paginatedTables[table] = targetPaginationKey
	}

	return
}

func LoadTables(db *sql.DB, tableFilter TableFilter, columnCompressionConfig ColumnCompressionConfig, columnIgnoreConfig ColumnIgnoreConfig, cascadingPaginationColumnConfig *CascadingPaginationColumnConfig) (TableSchemaCache, error) {
	logger := logrus.WithField("tag", "table_schema_cache")

	tableSchemaCache := make(TableSchemaCache)

	dbnames, err := showDatabases(db)
	if err != nil {
		logger.WithError(err).Error("failed to show databases")
		return tableSchemaCache, err
	}

	dbnames, err = tableFilter.ApplicableDatabases(dbnames)
	if err != nil {
		logger.WithError(err).Error("could not apply database filter")
		return tableSchemaCache, err
	}

	// For each database, get a list of tables from it and cache the table's schema
	for _, dbname := range dbnames {
		dbLog := logger.WithField("database", dbname)
		dbLog.Debug("loading tables from database")
		tableNames, err := showTablesFrom(db, dbname)
		if err != nil {
			dbLog.WithError(err).Error("failed to show tables")
			return tableSchemaCache, err
		}

		var tableSchemas []*TableSchema

		for _, table := range tableNames {
			tableLog := dbLog.WithField("table", table)
			tableLog.Debug("fetching table schema")
			tableSchema, err := schema.NewTableFromSqlDB(db.DB, dbname, table)
			if err != nil {
				tableLog.WithError(err).Error("cannot fetch table schema from source db")
				return tableSchemaCache, err
			}

			tableSchemas = append(tableSchemas, &TableSchema{
				Table:                            tableSchema,
				CompressedColumnsForVerification: columnCompressionConfig.CompressedColumnsFor(dbname, table),
				IgnoredColumnsForVerification:    columnIgnoreConfig.IgnoredColumnsFor(dbname, table),
			})
		}

		tableSchemas, err = tableFilter.ApplicableTables(tableSchemas)
		if err != nil {
			return tableSchemaCache, nil
		}

		for _, tableSchema := range tableSchemas {
			tableName := tableSchema.Name
			tableLog := dbLog.WithField("table", tableName)
			tableLog.Debug("caching table schema")

			if cascadingPaginationColumnConfig.IsFullCopyTable(tableSchema.Schema, tableName) {
				tableLog.Debug("table is marked for full-table copy")
			} else {
				tableLog.Debug("loading table schema pagination keys")
				paginationKey, err := tableSchema.paginationKey(cascadingPaginationColumnConfig)
				if err != nil {
					tableLog.WithError(err).Error("invalid table")
					return tableSchemaCache, err
				}
				tableLog.Debugf("using pagination key %s", paginationKey)
				tableSchema.PaginationKey = paginationKey
			}

			tableSchemaCache[tableSchema.String()] = tableSchema
		}
	}

	logger.WithField("tables", tableSchemaCache.AllTableNames()).Info("table schemas cached")

	return tableSchemaCache, nil
}

func (t *TableSchema) findColumnByName(name string) (*schema.TableColumn, int, error) {
	for i, column := range t.Columns {
		if column.Name == name {
			return &column, i, nil
		}
	}
	return nil, -1, NonExistingPaginationKeyColumnError(t.Schema, t.Name, name)
}

// NonExistingPaginationKeyColumnError exported to facilitate black box testing
func NonExistingPaginationKeyColumnError(schema, table, paginationKey string) error {
	return fmt.Errorf("Pagination Key `%s` for %s non existent", paginationKey, QuotedTableNameFromString(schema, table))
}

// NonExistingPaginationKeyError exported to facilitate black box testing
func NonExistingPaginationKeyError(schema, table string) error {
	return fmt.Errorf("%s has no Primary Key to default to for Pagination purposes. Kindly specify a Pagination Key for this table in the CascadingPaginationColumnConfig", QuotedTableNameFromString(schema, table))
}

// UnsupportedPaginationKeyError exported to facilitate black box testing
func UnsupportedPaginationKeyError(schema, table, paginationKey string) error {
	return fmt.Errorf("Pagination Key `%s` for %s is non-numeric/-text", paginationKey, QuotedTableNameFromString(schema, table))
}

func (t *TableSchema) paginationKey(cascadingPaginationColumnConfig *CascadingPaginationColumnConfig) (*PaginationKey, error) {
	var err error

	paginationKeyColumns := make([]*schema.TableColumn, 0)
	paginationKeyColumnIndices := make([]int, 0)

	if paginationColumn, found := cascadingPaginationColumnConfig.PaginationColumnFor(t.Schema, t.Name); found {
		// Use per-schema, per-table pagination key from config
		var paginationKeyColumn *schema.TableColumn
		var paginationKeyIndex int
		paginationKeyColumn, paginationKeyIndex, err = t.findColumnByName(paginationColumn)
		if err == nil {
			paginationKeyColumns = append(paginationKeyColumns, paginationKeyColumn)
			paginationKeyColumnIndices = append(paginationKeyColumnIndices, paginationKeyIndex)
		}
	} else if len(t.PKColumns) > 0 {
		// Use Primary Key
		//
		// NOTE: A primary key has to be unique, but it may contain columns that
		// are not needed for uniqueness. The ideal pagination key has length of
		// 1, so we explicitly check if a subset of keys is sufficient.
		// We could also do this checking for a uniqueness contstraint in the
		// future
		for i, column := range t.Columns {
			if column.IsAuto {
				paginationKeyColumns = append(paginationKeyColumns, &column)
				paginationKeyColumnIndices = append(paginationKeyColumnIndices, i)
				break
			}
		}
		// if we failed finding an auto-increment, build a composite key
		if len(paginationKeyColumns) == 0 {
			for _, paginationKeyIndex := range t.PKColumns {
				paginationKeyColumns = append(paginationKeyColumns, &t.Columns[paginationKeyIndex])
				paginationKeyColumnIndices = append(paginationKeyColumnIndices, paginationKeyIndex)
			}
		}
	} else if fallbackColumnName, found := cascadingPaginationColumnConfig.FallbackPaginationColumnName(); found {
		// Try fallback from config
		var paginationKeyColumn *schema.TableColumn
		var paginationKeyIndex int
		paginationKeyColumn, paginationKeyIndex, err = t.findColumnByName(fallbackColumnName)
		if err == nil {
			paginationKeyColumns = append(paginationKeyColumns, paginationKeyColumn)
			paginationKeyColumnIndices = append(paginationKeyColumnIndices, paginationKeyIndex)
		}
	} else {
		// No usable pagination key found
		err = NonExistingPaginationKeyError(t.Schema, t.Name)
	}

	if err != nil || len(paginationKeyColumns) == 0 {
		if err == nil {
			panic(fmt.Errorf("no pagination key found, but no error set either"))
		}
		return nil, err
	}

	for _, column := range paginationKeyColumns {
		switch column.Type {
		case schema.TYPE_NUMBER, schema.TYPE_STRING, schema.TYPE_VARBINARY, schema.TYPE_BINARY:
		default:
			return nil, UnsupportedPaginationKeyError(t.Schema, t.Name, column.Name)
		}
	}

	paginationKey := &PaginationKey{
		Columns: paginationKeyColumns,
		ColumnIndices: paginationKeyColumnIndices,
	}
	return paginationKey, err
}

func (c TableSchemaCache) AsSlice() (tables []*TableSchema) {
	for _, tableSchema := range c {
		tables = append(tables, tableSchema)
	}

	return
}

func (c TableSchemaCache) AllTableNames() (tableNames []string) {
	for tableName, _ := range c {
		tableNames = append(tableNames, tableName)
	}

	return
}

func (c TableSchemaCache) Get(database, table string) *TableSchema {
	return c[fullTableName(database, table)]
}

// Helper to sort a given map of tables with a second list giving a priority.
// If an element is present in the input and the priority lists, the item will
// appear first (in the order of the priority list), all other items appear in
// the order given in the input
func (c TableSchemaCache) GetTableListWithPriority(priorityList []string) (prioritzedTableNames []string) {
	// just a fast lookup if the list contains items already
	contains := map[string]struct{}{}
	if len(priorityList) >= 0 {
		for _, tableName := range priorityList {
			// ignore tables given in the priority list that we don't know
			if _, found := c[tableName]; found {
				contains[tableName] = struct{}{}
				prioritzedTableNames = append(prioritzedTableNames, tableName)
			}
		}
	}
	for tableName, _ := range c {
		if _, found := contains[tableName]; !found {
			prioritzedTableNames = append(prioritzedTableNames, tableName)
		}
	}

	return
}

// Helper to sort the given map of tables based on the dependencies between
// tables in terms of foreign key constraints
func (c TableSchemaCache) GetTableCreationOrder(db *sql.DB) (prioritzedTableNames []string, err error) {
	logger := logrus.WithField("tag", "table_schema_cache")

	tableReferences := make(map[QualifiedTableName]TableForeignKeys)
	for tableName, _ := range c {
		t := strings.Split(tableName, ".")
		table := NewQualifiedTableName(t[0], t[1])

		// ignore self-references, as they are not really foreign keys
		referencedTables, dbErr := GetForeignKeyTablesOfTable(db, table, false)
		if dbErr != nil {
			logger.WithField("table", table).Error("cannot analyze database table foreign keys")
			err = dbErr
			return
		}

		logger.Debugf("found %d reference tables for %s", len(referencedTables), table)
		tableReferences[table] = referencedTables
	}

	// simple fix-point loop: make sure we create at least one table per
	// iteration and mark tables as able to create as soon as they no-longer
	// refer to other tables
	for len(tableReferences) > 0 {
		createdTable := false
		for table, referencedTables := range tableReferences {
			if len(referencedTables) > 0 {
				continue
			}
			logger.Debugf("queuing %s", table)
			prioritzedTableNames = append(prioritzedTableNames, table.String())

			// mark any table referring to the table as potential candidates
			// for being created now
			for otherTable, otherReferencedTables := range tableReferences {
				if _, found := otherReferencedTables[table]; found {
					delete(otherReferencedTables, table)
					if len(otherReferencedTables) == 0 {
						logger.Debugf("creation of %s unblocked creation of %s", table, otherTable)
					}
				}

			}

			delete(tableReferences, table)
			createdTable = true
		}

		if !createdTable {
			tableNames := make([]QualifiedTableName, 0, len(tableReferences))
			for tableName := range tableReferences {
				tableNames = append(tableNames, tableName)
			}
			err = fmt.Errorf("failed creating tables: all %d remaining tables have foreign references: %v", len(tableReferences), tableNames)
			return
		}
	}

	return
}

func showDatabases(c *sql.DB) ([]string, error) {
	rows, err := c.Query("show databases")
	if err != nil {
		return []string{}, err
	}

	defer rows.Close()

	databases := make([]string, 0)
	for rows.Next() {
		var database string
		err = rows.Scan(&database)
		if err != nil {
			return databases, err
		}

		if _, ignored := ignoredDatabases[database]; ignored {
			continue
		}

		databases = append(databases, database)
	}

	return databases, nil
}

func showTablesFrom(c *sql.DB, dbname string) ([]string, error) {
	rows, err := c.Query(fmt.Sprintf("show tables from %s", quoteField(dbname)))
	if err != nil {
		return []string{}, err
	}
	defer rows.Close()

	tables := make([]string, 0)
	for rows.Next() {
		var table string
		err = rows.Scan(&table)
		if err != nil {
			return tables, err
		}

		tables = append(tables, table)
	}

	return tables, nil
}

func targetPaginationKey(db *sql.DB, table *TableSchema, iterateInDescendingOrder bool) (*PaginationKeyData, bool, error) {
	columnsToSelect := []string{"*"}

	selectBuilder, err := DefaultBuildSelect(columnsToSelect, table, nil, 1, !iterateInDescendingOrder)
	if err != nil {
		return nil, false, err
	}

	query, args, err := selectBuilder.ToSql()
	if err != nil {
		return nil, false, err
	}

	rows, err := db.Query(query, args...)

	if err != nil {
		return nil, false, err
	}

	defer rows.Close()
	if !rows.Next() {
		return nil, false, nil
	}

	rowData, err := ScanGenericRow(rows, len(table.Columns))
	if err != nil {
		return nil, false, err
	}

	paginationKeyData, err := NewPaginationKeyDataFromRow(rowData, table.PaginationKey)
	return paginationKeyData, true, err
}

func isEmptyTable(db *sql.DB, table *TableSchema) (bool, error) {
	query, args, err := sq.
		Select("1").
		From(QuotedTableName(table)).
		Limit(1).
		ToSql()

	if err != nil {
		return false, err
	}

	var dummy uint64
	err = db.QueryRow(query, args...).Scan(&dummy)
	if err == sqlorig.ErrNoRows {
		return true, nil
	}
	return false, err
}
