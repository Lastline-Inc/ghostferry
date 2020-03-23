package ghostferry

import (
	"fmt"
	sql "github.com/Shopify/ghostferry/sqlwrapper"

	"github.com/sirupsen/logrus"
)

type BatchWriterVerificationFailed struct {
	mismatchedPaginationKeys []uint64
	table                    string
}

func (e BatchWriterVerificationFailed) Error() string {
	return fmt.Sprintf("row fingerprints for paginationKeys %v on %v do not match", e.mismatchedPaginationKeys, e.table)
}

type BatchWriter struct {
	DB             *sql.DB
	InlineVerifier *InlineVerifier
	StateTracker   *StateTracker

	DatabaseRewrites map[string]string
	TableRewrites    map[string]string

	WriteRetries int

	stmtCache *StmtCache
	logger    *logrus.Entry
}

func (w *BatchWriter) Initialize() {
	w.stmtCache = NewStmtCache()
	w.logger = logrus.WithField("tag", "batch_writer")
}

func (w *BatchWriter) WriteRowBatch(batch RowBatch) error {
	return WithRetries(w.WriteRetries, 0, w.logger, "write batch to target", func() error {
		db := batch.TableSchema().Schema
		if targetDbName, exists := w.DatabaseRewrites[db]; exists {
			db = targetDbName
		}

		table := batch.TableSchema().Name
		if targetTableName, exists := w.TableRewrites[table]; exists {
			table = targetTableName
		}

		switch b := batch.(type) {
		case InsertRowBatch:
			return w.writeInsertRowBatch(b, db, table)
		// NOTE: It's important we check this last, as the interface
		// is (currently) indistinguishable from a plain RowBatch
		case InitRowBatch:
			return w.writeInitRowBatch(b, db, table)
		default:
			// see above, we can actually never get here right now
			return fmt.Errorf("unsupported row-batch type %T", batch)
		}
	})
}

func (w *BatchWriter) writeInsertRowBatch(batch InsertRowBatch, db, table string) error {
	values := batch.Values()
	if len(values) == 0 {
		return nil
	}

	var startPaginationKeypos, endPaginationKeypos uint64
	var err error
	if batch.ValuesContainPaginationKey() {
		index := batch.PaginationKeyIndex()
		startPaginationKeypos, err = values[0].GetUint64(index)
		if err != nil {
			return err
		}

		endPaginationKeypos, err = values[len(values)-1].GetUint64(index)
		if err != nil {
			return err
		}
	}

	query, args, err := batch.AsSQLQuery(db, table)
	if err != nil {
		return fmt.Errorf("during generating sql query at paginationKey %v -> %v: %v", startPaginationKeypos, endPaginationKeypos, err)
	}

	stmt, err := w.stmtCache.StmtFor(w.DB, query)
	if err != nil {
		return fmt.Errorf("during prepare query near paginationKey %v -> %v (%s): %v", startPaginationKeypos, endPaginationKeypos, query, err)
	}

	tx, err := w.DB.Begin()
	if err != nil {
		return fmt.Errorf("unable to begin transaction in BatchWriter: %v", err)
	}

	_, err = tx.Stmt(stmt).Exec(args...)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("during exec query near paginationKey %v -> %v (%s): %v", startPaginationKeypos, endPaginationKeypos, query, err)
	}

	if w.InlineVerifier != nil {
		mismatches, err := w.InlineVerifier.CheckFingerprintInline(tx, db, table, batch)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("during fingerprint checking for paginationKey %v -> %v (%s): %v", startPaginationKeypos, endPaginationKeypos, query, err)
		}

		if len(mismatches) > 0 {
			tx.Rollback()
			return BatchWriterVerificationFailed{mismatches, batch.TableSchema().String()}
		}
	}

	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("during commit near paginationKey %v -> %v (%s): %v", startPaginationKeypos, endPaginationKeypos, query, err)
	}

	// Note that the state tracker expects us the track based on the original
	// database and table names as opposed to the target ones.
	if w.StateTracker != nil {
		w.StateTracker.UpdateLastSuccessfulPaginationKey(batch.TableSchema().String(), endPaginationKeypos)
	}

	return nil
}

func (w *BatchWriter) writeInitRowBatch(batch InitRowBatch, db, table string) error {
	query, args, err := batch.AsSQLQuery(db, table)
	if err != nil {
		return fmt.Errorf("during generating sql init query: %v", err)
	}

	stmt, err := w.stmtCache.StmtFor(w.DB, query)
	if err != nil {
		return fmt.Errorf("during prepare init query (%s): %v", query, err)
	}

	tx, err := w.DB.Begin()
	if err != nil {
		return fmt.Errorf("unable to begin transaction in BatchWriter: %v", err)
	}

	_, err = tx.Stmt(stmt).Exec(args...)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("during exec init query (%s): %v", query, err)
	}

	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("during init commit (%s): %v", query, err)
	}

	return nil
}
