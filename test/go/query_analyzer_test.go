package test

import (
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/Shopify/ghostferry"
)

type QueryAnalyzerTestSuite struct {
	suite.Suite
	*ghostferry.QueryAnalyzer
}

func (this *QueryAnalyzerTestSuite) SetupTest() {
	this.QueryAnalyzer = ghostferry.NewQueryAnalyzer()
	this.Require().NotNil(this.QueryAnalyzer)
}

func (this *QueryAnalyzerTestSuite) TestParseInvalidSQL() {
	events, err := this.QueryAnalyzer.ParseSchemaChanges("this is not sql", "")
	this.Require().NotNil(err)
	this.Require().Nil(events)
}

func (this *QueryAnalyzerTestSuite) TestParseCreateTableStatement() {
	inputSql := "CREATE TABLE `dbname`.`tablename` (`id` int)"
	events, err := this.QueryAnalyzer.ParseSchemaChanges(inputSql, "")
	this.Require().Nil(err)
	this.Require().Equal(len(events), 1)
	this.Require().True(events[0].IsSchemaChange)
	this.Require().Equal(events[0].SchemaStatement, inputSql)
	this.Require().Equal(events[0].CreatedTable.SchemaName, "dbname")
	this.Require().Equal(events[0].CreatedTable.TableName, "tablename")
	this.Require().Equal(events[0].AffectedTable, events[0].CreatedTable)
	this.Require().Nil(events[0].DeletedTable)
}

func (this *QueryAnalyzerTestSuite) TestParseGrantStatement() {
	events, err := this.QueryAnalyzer.ParseSchemaChanges("GRANT USAGE ON *.* TO `username`@`%`", "")
	this.Require().Nil(err)
	this.Require().Equal(len(events), 0)
}

func (this *QueryAnalyzerTestSuite) TestParseGrantStatementWithMetadata() {
	events, err := this.QueryAnalyzer.ParseSchemaChanges("GRANT USAGE ON *.* TO `username`@`%` WITH MAX_USER_CONNECTIONS 0 MAX_CONNECTIONS_PER_HOUR 0 MAX_QUERIES_PER_HOUR 0 MAX_UPDATES_PER_HOUR 0", "")
	this.Require().Nil(err)
	this.Require().Equal(len(events), 0)
}

func (this *QueryAnalyzerTestSuite) TestParseRevokeStatement() {
	events, err := this.QueryAnalyzer.ParseSchemaChanges("REVOKE ALL PRIVILEGES FROM 'username'@'%'", "")
	this.Require().Nil(err)
	this.Require().Equal(len(events), 0)
}

func (this *QueryAnalyzerTestSuite) TestParseRevokeStatementWithGrantOption() {
	events, err := this.QueryAnalyzer.ParseSchemaChanges("REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'username'@'%'", "")
	this.Require().Nil(err)
	this.Require().Equal(len(events), 0)
}

func TestQueryAnalyzer(t *testing.T) {
	suite.Run(t, new(QueryAnalyzerTestSuite))
}
