package test

import (
	"fmt"
	"testing"

	"github.com/Shopify/ghostferry"
	"github.com/Shopify/ghostferry/testhelpers"
	"github.com/siddontang/go-mysql/mysql"
	"github.com/stretchr/testify/suite"
)

const (
	StateSchemaName = "gftest_state"
)

type StateTrackerTestSuite struct {
	*testhelpers.GhostferryUnitTestSuite
}

func (this *StateTrackerTestSuite) SetupTest() {
	this.GhostferryUnitTestSuite.SetupTest()
	this.resetDbs()
}

func (this *StateTrackerTestSuite) TearDownTest() {
	this.resetDbs()
	this.GhostferryUnitTestSuite.TearDownTest()
}

func (this *StateTrackerTestSuite) resetDbs() {
	_, err := this.Ferry.TargetDB.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", StateSchemaName))
	this.Require().Nil(err)
}

func (s *StateTrackerTestSuite) TestMinBinlogPosition() {
	serializedState := &ghostferry.SerializableState{
		LastWrittenBinlogPosition: ghostferry.NewResumableBinlogPosition(mysql.Position{
			Name: "mysql-bin.00003",
			Pos:  4,
		}),

		LastStoredBinlogPositionForInlineVerifier: ghostferry.NewResumableBinlogPosition(mysql.Position{
			Name: "mysql-bin.00003",
			Pos:  10,
		}),
	}
	s.Require().Equal(serializedState.MinBinlogPosition().EventPosition, mysql.Position{"mysql-bin.00003", 4})

	serializedState = &ghostferry.SerializableState{
		LastWrittenBinlogPosition: ghostferry.NewResumableBinlogPosition(mysql.Position{
			Name: "mysql-bin.00003",
			Pos:  4,
		}),

		LastStoredBinlogPositionForInlineVerifier: ghostferry.NewResumableBinlogPosition(mysql.Position{
			Name: "mysql-bin.00002",
			Pos:  10,
		}),
	}
	s.Require().Equal(serializedState.MinBinlogPosition().EventPosition, mysql.Position{"mysql-bin.00002", 10})

	serializedState = &ghostferry.SerializableState{
		LastWrittenBinlogPosition: ghostferry.NewResumableBinlogPosition(mysql.Position{
			Name: "",
			Pos:  0,
		}),

		LastStoredBinlogPositionForInlineVerifier: ghostferry.NewResumableBinlogPosition(mysql.Position{
			Name: "mysql-bin.00002",
			Pos:  10,
		}),
	}
	s.Require().Equal(serializedState.MinBinlogPosition().EventPosition, mysql.Position{"mysql-bin.00002", 10})

	serializedState = &ghostferry.SerializableState{
		LastStoredBinlogPositionForInlineVerifier: ghostferry.NewResumableBinlogPosition(mysql.Position{
			Name: "",
			Pos:  0,
		}),

		LastWrittenBinlogPosition: ghostferry.NewResumableBinlogPosition(mysql.Position{
			Name: "mysql-bin.00002",
			Pos:  10,
		}),
	}
	s.Require().Equal(serializedState.MinBinlogPosition().EventPosition, mysql.Position{"mysql-bin.00002", 10})
}

func (s *StateTrackerTestSuite) TestSerializeStateInTargetDB() {
	testFerry := s.TestFerry.Ferry
	testFerry.ResumeStateFromDB = StateSchemaName

	stateTracker1, _, err := ghostferry.NewStateTrackerFromTargetDB(testFerry)
	s.Require().Nil(err)

	// make sure the state table was created
	rows, err := testFerry.TargetDB.Query(fmt.Sprintf("SELECT * FROM `%s`.`_ghostferry_91919__last_binlog_writer_state`", StateSchemaName))
	s.Require().Nil(err)
	defer rows.Close()
	s.Require().True(rows.Next())

	state1 := stateTracker1.Serialize(nil, nil)

	// now advance the master position and make sure such changes are
	// reflected in the state
	s.SeedSourceDB(1)
	// this is just to make sure that the below code tests what it should: it
	// did not read the position from the source DB, but from the state tables
	// on the target DB
	masterPos, err := ghostferry.ShowMasterStatusBinlogPosition(testFerry.SourceDB)
	s.Require().Nil(err)
	s.Require().True(masterPos.Compare(state1.LastWrittenBinlogPosition.ResumePosition) > 0)

	stateTracker2, state2, err := ghostferry.NewStateTrackerFromTargetDB(testFerry)
	s.Require().Nil(err)
	s.Require().Equal(state1.LastWrittenBinlogPosition, state2.LastWrittenBinlogPosition)

	// make sure that the state is not only reflected in the returned state
	// object, but also in the state-tracker itself
	state3 := stateTracker2.Serialize(nil, nil)
	s.Require().Equal(state2.LastWrittenBinlogPosition, state3.LastWrittenBinlogPosition)
}

func (s *StateTrackerTestSuite) TestReadStateFromTargetDBContainingUnknownTable() {
	testFerry := s.TestFerry.Ferry
	testFerry.ResumeStateFromDB = StateSchemaName

	// initialize the state
	_, _, err := ghostferry.NewStateTrackerFromTargetDB(testFerry)
	s.Require().Nil(err)

	// corrupt the state by adding an unknown table
	//
	// NOTE: If we don't correctly ignore the unknown table, we should crash
	// due to the key data not being valid JSON
	query := fmt.Sprintf("INSERT INTO %s.`_ghostferry_91919__row_copy_state` (table_name, last_pagination_key) VALUES ('%s.unknown_table', 'not json')", StateSchemaName, StateSchemaName)
	_, err = testFerry.TargetDB.Exec(query)
	testhelpers.PanicIfError(err)

	// make sure loading still works
	stateTracker, _, err := ghostferry.NewStateTrackerFromTargetDB(testFerry)
	s.Require().Nil(err)
	s.Require().NotNil(stateTracker)
}

func (s *StateTrackerTestSuite) TestReadStateFromTargetDBContainingCorruptedKeyData() {
	testFerry := s.TestFerry.Ferry
	testFerry.ResumeStateFromDB = StateSchemaName

	s.SeedSourceDB(0)
	tableFilter := &testhelpers.TestTableFilter{
		DbsFunc:    testhelpers.DbApplicabilityFilter([]string{testhelpers.TestSchemaName}),
		TablesFunc: nil,
	}
	testFerry.Tables, _ = ghostferry.LoadTables(testFerry.SourceDB, tableFilter, nil, nil, nil)

	// initialize the state
	_, _, err := ghostferry.NewStateTrackerFromTargetDB(testFerry)
	s.Require().Nil(err)

	// corrupt the state in a way that loading should raise an error
	query := fmt.Sprintf("INSERT INTO %s.`_ghostferry_91919__row_copy_state` (table_name, last_pagination_key) VALUES ('%s.%s', 'not json')", StateSchemaName, testhelpers.TestSchemaName, testhelpers.TestTable1Name)
	_, err = testFerry.TargetDB.Exec(query)
	testhelpers.PanicIfError(err)

	// make sure loading still works
	_, _, err = ghostferry.NewStateTrackerFromTargetDB(testFerry)
	s.Require().NotNil(err)
	s.Require().EqualError(err, fmt.Sprintf("invalid character 'o' in literal null (expecting 'u')"))
}

func TestStateTrackerTestSuite(t *testing.T) {
	testhelpers.SetupTest()
	suite.Run(t, &StateTrackerTestSuite{GhostferryUnitTestSuite: &testhelpers.GhostferryUnitTestSuite{}})
}
