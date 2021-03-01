package ghostferry

import (
	"bytes"
	sqlorig "database/sql"
	"fmt"
	sql "github.com/Shopify/ghostferry/sqlwrapper"
	"strings"
	"sync"

	"github.com/Masterminds/squirrel"
	"github.com/siddontang/go-mysql/schema"
	"github.com/sirupsen/logrus"
)

// both `sql.Tx` and `sql.DB` allow a SQL query to be `Prepare`d
type SqlPreparer interface {
	Prepare(string) (*sqlorig.Stmt, error)
}

type SqlDBWithFakeRollback struct {
	*sql.DB
	lock *sync.RWMutex
}

func NewSqlDBWithFakeRollback(db *sql.DB, lock *sync.RWMutex) *SqlDBWithFakeRollback {
	tx := &SqlDBWithFakeRollback{
		DB:   db,
		lock: lock,
	}
	if lock != nil {
		lock.Lock()
	}
	return tx
}

func (d *SqlDBWithFakeRollback) Rollback() error {
	if d.lock != nil {
		d.lock.Unlock()
	}
	return nil
}

// sql.DB does not implement Rollback, but can use SqlDBWithFakeRollback
// to perform a noop.
type SqlPreparerAndRollbacker interface {
	SqlPreparer
	Query(query string, args ...interface{}) (*sqlorig.Rows, error)
	Rollback() error
}

type PaginationKeyData struct {
	// The Values is the subset of column values of a full row that makes up
	// the pagination key. The list is stored in the same way as the pagination
	// key column itself
	Values        RowData
	paginationKey *PaginationKey
}

func NewPaginationKeyDataFromRow(row RowData, paginationKey *PaginationKey) (paginationKeyData *PaginationKeyData, err error) {
	values := make(RowData, len(paginationKey.Columns))
	for i, column := range paginationKey.Columns {
		// NOTE: The data we get from the MySQL driver can be all over the
		// place, so it's really important we know what we are casting the
		// value to
		colIdx :=  paginationKey.ColumnIndices[i]
		if column.Type == schema.TYPE_NUMBER {
			value, err := row.GetInt64(colIdx)
			if err != nil {
				return nil, err
			}
			values[i] = value
		} else if column.Type == schema.TYPE_STRING {
			values[i] = row.GetString(colIdx)
		} else if column.Type == schema.TYPE_BINARY || column.Type == schema.TYPE_VARBINARY {
			// we special-case binary/varbinary, as we want to copying them into a
			// string and directly operate on the byte stream
			if value, ok := row[colIdx].([]byte); ok {
				values[i] = value
			} else {
				values[i] = row.GetString(colIdx)
			}
		} else {
			value := row[colIdx]
			return nil, fmt.Errorf("unsupported primary key type %T (%v) in %s", value, value, paginationKey)
		}
	}
	paginationKeyData = &PaginationKeyData{
		Values:        values,
		paginationKey: paginationKey,
	}

	return
}

func UnmarshalPaginationKeyData(keyData *PaginationKeyData, table *TableSchema) (paginationKeyData *PaginationKeyData, err error) {
	// build a dummy row of the original table, filled in with the pagination
	// key data, which we then import using the regular method above
	if len(keyData.Values) != len(table.PaginationKey.ColumnIndices) {
		return nil, fmt.Errorf("unmarshalling invalid values for %s on table %s: expecting %d values, got %d", table.PaginationKey, table, len(table.PaginationKey.ColumnIndices), len(keyData.Values))
	}
	row := make(RowData, len(table.Columns))
	for i, columnIndex := range table.PaginationKey.ColumnIndices {
		value := keyData.Values[i]
		// we need to support a few more types that the JSON unmarshalling
		// infers incorrectly
		switch v := value.(type) {
		case float64:
			value = int(v)
		case float32:
			value = int(v)
		}
		row[columnIndex] = value
	}
	return NewPaginationKeyDataFromRow(row, table.PaginationKey)
}

func (d PaginationKeyData) String() string {
	s := ""
	if d.Values != nil {
		for i, value := range d.Values {
			if i > 0 {
				s += ","
			}
			var format string
			switch value.(type) {
			case int:
				format = "%d"
			case string:
				format = "%s"
			default:
				format = "%v"
			}
			s += fmt.Sprintf(format, value)
		}
	}
	return s
}

// for some types of keys, we can estimate the progress of pagination by
// comparing the most significant part of the key to the target pagination
// value. It's only a rough estimate, as it assume a linear distribution of
// values between 0 and the target (no negative values, no holes/jumps, etc)
// and cannot work for non-integer keys, but it's nevertheless useful
func (d PaginationKeyData) ProgressData() (progress uint64, exists bool) {
	if d.Values == nil || d.paginationKey.MostSignificantColumnIndex < 0 {
		return 0, false
	}

	value, ok := d.Values[d.paginationKey.MostSignificantColumnIndex].(int64)
	if ok && value >= 0 {
		return uint64(value), true
	}
	return 0, false
}

func (d *PaginationKeyData) Compare(other *PaginationKeyData) int {
	// mimic comparisons of uninitialized structures as we would for
	// integers (as it was before we supported composite keys): nil
	// values are equal to themselves, and an uninitialized value is
	// always lower than an initialized one
	if d == nil {
		if other == nil {
			return 0
		}
		return -1
	} else if other == nil {
		return 1
	}

	// NOTE: This works because we SELECT ... ORDER BY by the same ordering
	for i, value := range d.Values {
		if dValue, ok := value.(int64); ok {
			otherValue, ok := other.Values[i].(int64)
			if !ok {
				panic(fmt.Errorf("comparing incompatible primary key types %T and %T", d.Values[i], other.Values[i]))
			}
			if dValue < otherValue {
				return -1
			} else if dValue > otherValue {
				return 1
			}
		} else if bValue, ok := value.([]byte); ok {
			otherValue, ok := other.Values[i].([]byte)
			if !ok {
				panic(fmt.Errorf("comparing incompatible primary key types %T and %T", d.Values[i], other.Values[i]))
			}
			cmp := bytes.Compare(bValue, otherValue)
			if cmp != 0 {
				return cmp
			}
		} else if dValue, ok := value.(string); ok {
			otherValue, ok := other.Values[i].(string)
			if !ok {
				panic(fmt.Errorf("comparing incompatible primary key types %T and %T", d.Values[i], other.Values[i]))
			}
			if dValue < otherValue {
				return -1
			} else if dValue > otherValue {
				return 1
			}
		} else {
			// panic, because we should never have gotten here, as we validate
			// on struct creation
			panic(fmt.Errorf("unsupported primary key type %T ('%v') in %s", value, value, d))
		}
	}
	return 0
}

type CursorConfig struct {
	DB        *sql.DB
	Throttler Throttler

	ColumnsToSelect []string
	BuildSelect     func([]string, *TableSchema, *PaginationKeyData, uint64, bool) (squirrel.SelectBuilder, error)
	BatchSize       uint64
	ReadRetries     int

	IterateInDescendingOrder bool
}

// returns a new PaginatedCursor with an embedded copy of itself
func (c *CursorConfig) NewPaginatedCursor(table *TableSchema, startPaginationKey, maxPaginationKey *PaginationKeyData) *PaginatedCursor {
	return &PaginatedCursor{
		CursorConfig:                *c,
		Table:                       table,
		MaxPaginationKey:            maxPaginationKey,
		RowLock:                     true,
		lastSuccessfulPaginationKey: startPaginationKey,
	}
}

// returns a new PaginatedCursor with an embedded copy of itself
func (c *CursorConfig) NewPaginatedCursorWithoutRowLock(table *TableSchema, startPaginationKey, maxPaginationKey *PaginationKeyData, tableLock *sync.RWMutex) *PaginatedCursor {
	cursor := c.NewPaginatedCursor(table, startPaginationKey, maxPaginationKey)
	cursor.RowLock = false
	// NOTE: We only allow internal table locking, if row-locking is disabled
	// to avoid a potential deadlock
	cursor.tableLock = tableLock
	return cursor
}

type PaginatedCursor struct {
	CursorConfig

	Table            *TableSchema
	MaxPaginationKey *PaginationKeyData
	RowLock          bool

	paginationKeyColumn         *PaginationKey
	lastSuccessfulPaginationKey *PaginationKeyData
	tableLock                   *sync.RWMutex
	logger                      *logrus.Entry
}

func (c *PaginatedCursor) Each(f func(RowBatch) error) error {
	c.logger = logrus.WithFields(logrus.Fields{
		"table": c.Table.String(),
		"tag":   "cursor",
	})
	c.paginationKeyColumn = c.Table.PaginationKey

	if len(c.ColumnsToSelect) == 0 {
		c.ColumnsToSelect = []string{"*"}
	}

	for {
		if c.lastSuccessfulPaginationKey != nil {
			status := c.lastSuccessfulPaginationKey.Compare(c.MaxPaginationKey)
			if c.IterateInDescendingOrder && status <= 0 || !c.IterateInDescendingOrder && status >= 0 {
				break
			}
		}

		var tx SqlPreparerAndRollbacker
		var batch InsertRowBatch
		var paginationKeypos *PaginationKeyData

		err := WithRetries(c.ReadRetries, 0, c.logger, "fetch rows", func() (err error) {
			if c.Throttler != nil {
				WaitForThrottle(c.Throttler)
			}

			// Only need to use a transaction if RowLock == true. Otherwise
			// we'd be wasting two extra round trips per batch, doing
			// essentially a no-op.
			if c.RowLock {
				tx, err = c.DB.Begin()
				if err != nil {
					return err
				}
			} else {
				tx = NewSqlDBWithFakeRollback(c.DB, c.tableLock)
			}

			batch, paginationKeypos, err = c.Fetch(tx)
			if err == nil {
				return nil
			}

			tx.Rollback()
			return err
		})

		if err != nil {
			return err
		}

		if batch.Size() == 0 {
			tx.Rollback()
			c.logger.Debug("did not reach max primary key, but the table is complete as there are no more rows")
			break
		}

		if c.lastSuccessfulPaginationKey != nil {
			progress := paginationKeypos.Compare(c.lastSuccessfulPaginationKey)
			if c.IterateInDescendingOrder && progress >= 0 || !c.IterateInDescendingOrder && progress <= 0 {
				tx.Rollback()
				failedOperator := "<="
				if c.IterateInDescendingOrder {
					failedOperator = ">="
				}
				err = fmt.Errorf("new %s paginationKeypos %s %s lastSuccessfulPaginationKey %s (%d)", c.Table, paginationKeypos, failedOperator, c.lastSuccessfulPaginationKey, progress)
				c.logger.WithError(err).Errorf("last successful paginationKey position did not advance")
				return err
			}
		}

		err = f(batch)
		if err != nil {
			tx.Rollback()
			c.logger.WithError(err).Error("failed to call each callback")
			return err
		}

		tx.Rollback()

		c.lastSuccessfulPaginationKey = paginationKeypos
	}

	// notify all listeners that the copy is done. We use a dedicated event for
	// this to allow listeners an optimized way to look just for this, but also
	// to avoid corner-cases where tables are empty to begin with or if we end
	// pagination exactly at a batch-boundary
	finalizeBatch := NewFinalizeTableCopyBatch(c.Table)
	err := f(finalizeBatch)
	if err != nil {
		c.logger.WithError(err).Error("failed to call finish-each callback")
		return err
	}

	return nil
}

func (c *PaginatedCursor) Fetch(db SqlPreparer) (batch InsertRowBatch, paginationKeyData *PaginationKeyData, err error) {
	var selectBuilder squirrel.SelectBuilder

	if c.BuildSelect != nil {
		selectBuilder, err = c.BuildSelect(c.ColumnsToSelect, c.Table, c.lastSuccessfulPaginationKey, c.BatchSize, c.IterateInDescendingOrder)
	} else {
		selectBuilder, err = DefaultBuildSelect(c.ColumnsToSelect, c.Table, c.lastSuccessfulPaginationKey, c.BatchSize, c.IterateInDescendingOrder)
	}
	if err != nil {
		c.logger.WithError(err).Error("failed to apply filter for select")
		return
	}

	if c.RowLock {
		selectBuilder = selectBuilder.Suffix("FOR UPDATE")
	}

	query, args, err := selectBuilder.ToSql()
	if err != nil {
		c.logger.WithError(err).Error("failed to build chunking sql")
		return
	}

	// With the inline verifier, the columns to be selected may be very large as
	// the query generated will be very large. The code here simply hides the
	// columns from the logger to not spam the logs.

	splitQuery := strings.Split(query, "FROM")
	loggedQuery := fmt.Sprintf("SELECT [omitted] FROM %s", splitQuery[1])

	logger := c.logger.WithFields(logrus.Fields{
		"sql":  loggedQuery,
		"args": args,
	})
	if IncrediblyVerboseLogging {
		logger.Debugf("full query: %s [%v]", query, args)
	}

	// This query must be a prepared query. If it is not, querying will use
	// MySQL's plain text interface, which will scan all values into []uint8
	// if we give it []interface{}.
	stmt, err := db.Prepare(query)
	if err != nil {
		logger.WithError(err).Error("failed to prepare query")
		return
	}

	defer stmt.Close()

	rows, err := stmt.Query(args...)
	if err != nil {
		logger.WithError(err).Error("failed to query database")
		return
	}

	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		logger.WithError(err).Error("failed to get columns")
		return
	}

	var rowData RowData
	var batchData []RowData

	for rows.Next() {
		rowData, err = ScanGenericRow(rows, len(columns))
		if err != nil {
			logger.WithError(err).Error("failed to scan row")
			return
		}

		batchData = append(batchData, rowData)
	}

	err = rows.Err()
	if err != nil {
		return
	}

	if len(batchData) > 0 {
		paginationKeyData, err = NewPaginationKeyDataFromRow(batchData[len(batchData)-1], c.paginationKeyColumn)
		if err != nil {
			logger.WithError(err).Error("failed to get paginationKey data")
			return
		}
	}

	batch = NewDataRowBatch(c.Table, batchData)
	logger.Debugf("found %d/%d rows", batch.Size(), c.BatchSize)

	return
}

// returns a new PaginatedCursor with an embedded copy of itself
func (c *CursorConfig) NewFullTableCursor(table *TableSchema, lockOnDB bool, tableLock *sync.RWMutex) *FullTableCursor {
	// NOTE: We only allow internal table locking, if row-locking is disabled
	// to avoid a potential deadlock
	if lockOnDB && tableLock != nil {
		panic("invalid configuration using DB and table locking")
	}

	return &FullTableCursor{
		DB:          c.DB,
		Table:       table,
		BatchSize:   c.BatchSize,
		ReadRetries: c.ReadRetries,
		lockOnDB:    lockOnDB,
		tableLock:   tableLock,
	}
}

type FullTableCursor struct {
	DB          *sql.DB
	Table       *TableSchema
	BatchSize   uint64
	ReadRetries int

	lockOnDB  bool
	tableLock *sync.RWMutex
	logger    *logrus.Entry
}

func (c *FullTableCursor) Each(f func(RowBatch) error) error {
	c.logger = logrus.WithFields(logrus.Fields{
		"table": c.Table.String(),
		"tag":   "fullTableCursor",
	})

	// we do not support pagination, so we cannot resume copying of full-table
	// copies. We need to send out a way to re-initialize and prepare for a
	// full-table copy
	initBatch := NewTruncateTableBatch(c.Table)
	err := f(initBatch)
	if err != nil {
		c.logger.WithError(err).Error("failed to call init-each callback")
		return err
	}

	err = WithRetries(c.ReadRetries, 0, c.logger, "fetch rows", func() (err error) {
		var tx SqlPreparerAndRollbacker
		if c.lockOnDB {
			tx, err = c.DB.Begin()
			if err != nil {
				return err
			}

			defer func() {
				_, err := tx.Query("UNLOCK TABLES")
				if err != nil {
					c.logger.WithError(err).Error("unlocking table failed")
				}
				tx.Rollback()
			}()

			// NOTE: We need to hold the row-lock on all rows for the entire
			// operation. Yes, crazy, but if we can't paginate, what are we supposed
			// to do? We really, *really*, *really* only use this for small tables
			_, err = tx.Query(fmt.Sprintf("LOCK TABLES %s WRITE", QuotedTableName(c.Table)))
			if err != nil {
				c.logger.WithError(err).Error("locking table failed")
				return err
			}
		} else {
			tx = &SqlDBWithFakeRollback{DB: c.DB}
			if c.tableLock != nil {
				c.tableLock.Lock()
				defer func() {
					c.tableLock.Unlock()
				}()
			}
		}

		rowOffset := 0
		for {
			c.logger.Debugf("fetching full-table batch at offset %d", rowOffset)
			batch, err := c.Fetch(tx, rowOffset)
			if err != nil {
				c.logger.WithError(err).Errorf("failed to invoke fetch at offset %d", rowOffset)
				return err
			}

			// NOTE: We propagate even an empty batch here to propagate the
			// fact that the copy is done (e.g., if the table was empty)
			err = f(batch)
			if err != nil {
				c.logger.WithError(err).Error("failed to call each callback")
				return err
			}

			if batch.Size() < int(c.BatchSize) {
				c.logger.Debugf("there are no more rows to copy: last batch contained %d/%d rows", batch.Size(), c.BatchSize)
				break
			}

			rowOffset += batch.Size()
		}

		// notify all listeners that the copy is done. We use a dedicated event for
		// this to allow listeners an optimized way to look just for this, but also
		// to avoid corner-cases where tables are empty to begin with or if we end
		// pagination exactly at a batch-boundary
		finalizeBatch := NewFinalizeTableCopyBatch(c.Table)
		err = f(finalizeBatch)
		if err != nil {
			c.logger.WithError(err).Error("failed to call finish-each callback")
			return err
		}

		return nil
	})

	return err
}

func (c *FullTableCursor) Fetch(db SqlPreparer, rowOffset int) (batch InsertRowBatch, err error) {
	// NOTE: The caller already locked the table for us
	selectBuilder := squirrel.Select("*").
		From(QuotedTableName(c.Table)).
		Limit(c.BatchSize).
		Offset(uint64(rowOffset))
	query, args, err := selectBuilder.ToSql()
	if err != nil {
		c.logger.WithError(err).Error("failed to build limit-offset sql")
		return
	}

	splitQuery := strings.Split(query, "FROM")
	loggedQuery := fmt.Sprintf("SELECT [omitted] FROM %s", splitQuery[1])

	logger := c.logger.WithFields(logrus.Fields{
		"sql":  loggedQuery,
		"args": args,
	})

	// This query must be a prepared query. If it is not, querying will use
	// MySQL's plain text interface, which will scan all values into []uint8
	// if we give it []interface{}.
	stmt, err := db.Prepare(query)
	if err != nil {
		logger.WithError(err).Error("failed to prepare query")
		return
	}

	defer stmt.Close()

	rows, err := stmt.Query(args...)
	if err != nil {
		logger.WithError(err).Error("failed to query database")
		return
	}

	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		logger.WithError(err).Error("failed to get columns")
		return
	}

	var rowData RowData
	var batchData []RowData

	for rows.Next() {
		rowData, err = ScanGenericRow(rows, len(columns))
		if err != nil {
			logger.WithError(err).Error("failed to scan row")
			return
		}

		batchData = append(batchData, rowData)
	}

	err = rows.Err()
	if err != nil {
		return
	}

	batch = NewDataRowBatch(c.Table, batchData)
	logger.Debugf("found %d/%d rows", batch.Size(), c.BatchSize)

	return
}

func ScanGenericRow(rows *sqlorig.Rows, columnCount int) (RowData, error) {
	values := make(RowData, columnCount)
	valuePtrs := make(RowData, columnCount)

	for i, _ := range values {
		valuePtrs[i] = &values[i]
	}

	err := rows.Scan(valuePtrs...)
	return values, err
}

func ScanByteRow(rows *sqlorig.Rows, columnCount int) ([][]byte, error) {
	values := make([][]byte, columnCount)
	valuePtrs := make(RowData, columnCount)

	for i, _ := range values {
		valuePtrs[i] = &values[i]
	}

	err := rows.Scan(valuePtrs...)
	return values, err
}

func DefaultBuildSelect(columns []string, table *TableSchema, lastPaginationKey *PaginationKeyData, batchSize uint64, sortDescending bool) (squirrel.SelectBuilder, error) {
	stmt := squirrel.Select(columns...).From(QuotedTableName(table))

	// selecting a resume position in the context of composite primary keys is
	// not entirely trivial: consider a composite key of A+B and the following
	// example table:
	//
	//    +---+---+
	//    | A | B |
	//    +---+---+
	//    | 1 | 1 |
	//    | 1 | 2 |
	//    | 1 | 3 |
	//    | 2 | 1 |
	//    | 3 | 4 |
	//    +---+---+
	//
	// and a batch-size of 2, we will want to copy batches
	//
	//    [(1,1), (1,2)]
	//    [(1,3), (2,1)]
	//
	// If we select a naive implementation that simply selects >= on all parts
	// of the composite key - e.g., for the second batch
	//
	//     A >= 1 AND B > 2
	//
	// or, using an offset,
	//
	//     A >= 1 AND B >= 2 OFFSET 1
	//
	// we would skip copying row with A=2.
	//
	// To work around this, we have to build a select using all parts of the
	// key, such as
	//
	//     A > 1 OR A == 1 AND B > 2
	//
	// to allow proper resuming
	if lastPaginationKey != nil {
		if len(lastPaginationKey.Values) != len(lastPaginationKey.paginationKey.ColumnIndices) {
			return stmt, fmt.Errorf("building select with invalid values for %s on table %s: expecting %d values, got %d", table.PaginationKey, table, len(lastPaginationKey.paginationKey.ColumnIndices), len(lastPaginationKey.Values))
		}

		// unfortunately squirrel does not allow to build structures very
		// nicely, so we have to build an actual prepared SQL statement
		whereSql := ""
		args := make([]interface{}, 0)
		for maxIndexToInclude, _ := range table.PaginationKey.Columns {
			// build an AND-connected term for all columns up to the limit
			andSql := ""
			for i, column := range table.PaginationKey.Columns {
				if i > maxIndexToInclude {
					break
				}

				if i > 0 {
					andSql += " AND "
				}

				andSql += quoteField(column.Name)
				if i < maxIndexToInclude {
					andSql += "="
				} else if sortDescending {
					andSql += "<"
				} else {
					andSql += ">"
				}
				andSql += "?"
				args = append(args, lastPaginationKey.Values[i])
			}
			// now connect the different terms using OR
			if whereSql != "" {
				whereSql += " OR "
			}
			whereSql += andSql
		}
		stmt = stmt.Where(whereSql, args...)
	}

	// NOTE: It's important to sort in the order of the pagination key columns.
	// This not only allows the correct resume but also for finding at what
	// point we can stop a copy (see the PaginationKey.Compare())
	orderBy := make([]string, len(table.PaginationKey.Columns))
	for i, column := range table.PaginationKey.Columns {
		orderBy[i] = quoteField(column.Name)
		if sortDescending {
			orderBy[i] += " DESC"
		}
	}

	return stmt.Limit(batchSize).OrderBy(orderBy...), nil
}
