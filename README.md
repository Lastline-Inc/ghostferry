Ghostferry
==========

Ghostferry is a library that enables you to selectively copy data from one mysql instance to another with minimal amount of downtime.

It is inspired by Github's [gh-ost](https://github.com/github/gh-ost),
although instead of copying data from and to the same database, Ghostferry
copies data from one database to another and have the ability to only
partially copy data.

There is an example application called ghostferry-copydb included (under the
`copydb` directory) that demonstrates this library by copying an entire
database from one machine to another.

Talk to us on IRC at [irc.freenode.net #ghostferry](https://webchat.freenode.net/?channels=#ghostferry).

- **Tutorial and General Documentations**: https://shopify.github.io/ghostferry
- Code documentations: https://godoc.org/github.com/Shopify/ghostferry

Stable Versus Experimental
--------------------------

This project is an *experimental* fork of the official
[ghostferry project](https://github.com/Shopify/ghostferry). We add various
features and fixes that have not (yet) made it into the upstream version.

While the changes added in this fork **are considered experimental** and must
only be used at your own risk, we use them on a daily basis and we consider
them stable for use in practice. Still, use at your own risk and opt for the original version if their features satisfy your needs.

If you discover bugs in any feature added in this fork, please open an issue
ticket with the details. If the issue is within the base system, open an issue
on the original project - we constantly monitor stable changes there and
incorporate them into this fork.

Features/fixes added in this fork include

- fix [data corruption for binary columns](https://github.com/Shopify/ghostferry/issues/157):
  this fix has not made it into upstream master yet.
- fix [failure to resume](https://github.com/Shopify/ghostferry/issues/156):
  this fix has not made it into upstream master yet.
- allow specifying [table creation order](https://github.com/Shopify/ghostferry/issues/161):
  `Ghostferry` does not allow copying tables using *foreign key constraints*,
  because it copies data in batches, which is likely to violate constraints,
  leading to failures during the copy phase.  
  The forked version allows specifying the order in which tables need to be
  created in the target database. This allows working around constraints in the
  setup phase. Additionally disabling foreign key constraint enforcement on the
  target database session/connection allows working around constraints during
  the copy phase.  
  Note that an *incomplete* execution of `Ghostferry`  will leave the database in
  an inconsistent state until the copy is resumed and completed.
- improved handling of [foreign key constraints](https://github.com/Shopify/ghostferry/issues/161):
  support infer the table creation order automatically if the database contains
  *foreign key constraints* and no manual order of tables is specified in the
  configuration.  
  Note that this merely automates part of the setup needed for supporting
  foreign key constraints. There are still several limitations in place for
  migrating such databases and the feature must be used with great care.
  Especially the use of database- or table-rewrites may introduce invalid target
  database states that are not recoverable.
- use `DBReadRetries` configuration setting also for retrying reading from the
  binlog server (instead of using a hardcoded retry limit of 5).
- more robust [handling of bigint column values](https://github.com/Shopify/ghostferry/issues/165):
  this fix has not made it into upstream master yet.
- support [writing resume/state data to file](https://github.com/Shopify/ghostferry/issues/163)
  instead of using *stdout*.
- support [writing resume/state data to target database](https://github.com/Shopify/ghostferry/issues/163).
- support [copying tables without "paging" primary keys](https://github.com/Shopify/ghostferry/issues/162):
  `Ghostferry` requires integer auto-increment primary keys for copying data.
  An optional feature in this fork allows marking tables for "full copy",
  allowing to copy tables that do not meet this primary key requirement. It is
  **strongly recommended** to use this feature with care and only on tables with
  few rows, as the copy process requires locking the entire table on the source
  database.
- support [schema modifications during the cutover phase](https://github.com/Lastline-Inc/ghostferry/issues/11):
  unlike the original version of `Ghostferry`, which ignores any DDL events (such
  as `CREATE`/`ALTER`/`DELETE TABLE` or `TRUNCATE TABLE` statements), this fork
  propagates such events from the source to the target database.  
  Note that there are a few restrictions/limitations with this:
    - schema changes occurring during the *copy phase* cannot be handled if the
      columns of a copy-in-progress table change. There are work-arounds in
      place to recover the copy process after a restart though (see
      `DelayDataIterationUntilBinlogWriterShutdown` and
      `FailOnFirstTableCopyError`).
    - data integrity verification is not supported once a schema is changed, as
      its current implementation assumes on generating hashes/fingerprints of
      data/table rows. As soon as schemas are modified, it is currently not
      possible to generate fingerprints that are consistent across different
      schemas on source and target DB.
    - database/table name rewrites are not supported, as we would need non-
      trivial rewrites of schema changing statements when tables are altered,
      renamed, created, or deleted.
    - `GRANT` statements are ignored. These are not part of the schema per-se,
      but it is still worth pointing out.
    - `CREATE PROCEDURE` and `CREATE FUNCTION` statements are currently not
      supported and are ignored as part of replication. Likewise, functions
      or procedures defined on the source before replication is started are
      not copied to the target DB.
- support [reading from (read-only) DB replica](https://github.com/Lastline-Inc/ghostferry/issues/22):
  allow using locks within `Ghostferry` (instead of on the source database) to
  avoid race conditions between the data copy and binlog replication. The
  upstream version of `Ghostferry` locks tables on the source, potentially
  interferring with the application. Furthermore, by using locks outside the
  DB, it is possible to replicate from read-only sources where locking is not
  an option (e.g., cloud-SQL).
- support [non-int primary keys](https://github.com/Lastline-Inc/ghostferry/issues/24):
  extend `Ghostferry` to support signed/unsigned integers, string, and
  composite primary keys (that use ints and strings). This vastly reduces the
  types of tables for which "full copy" is required.  
  Additionally support iterating over table rows to copy in descending order.
  This allows reading recent data first if the pagination key reflects
  chronological data.  
  Note that there are a few restrictions/limitations with this:
    - support for row verification has not yet been implemented at this point.
      Enabling inline/iterative verifiers causes an error at runtime if data
      is requested to be verified for an incompatible table.
    - because we support signed integer primary keys now, the maximum key value
      supported is now 2<sup>63</sup> (previously 2<sup>64</sup>). In practice
      it is unlikely to have DB key values of this size, and it is thus not
      configurable to provide the legacy behavior.
- more robust [disabling of inline-verifier](https://github.com/Shopify/ghostferry/issues/184):
  this fix has not made it into upstream master yet.
- support throttling of data migration separately from replication. This allows
  prioritizing the data replication over the copy of old data (or vice-versa).
- update SQL parser to the latest stable release to work around
  [issues](https://github.com/pingcap/parser/issues/1017) when parsing
  schema-changing commands in the binlogs.

Overview of How it Works
------------------------

An overview of Ghostferry's high-level design is expressed in the TLA+
specification, under the `tlaplus` directory. It maybe good to consult with
that as it has a concise definition. However, the specification might not be
entirely correct as proofs remain elusive.

On a high-level, Ghostferry is broken into several components, enabling it to
copy data. This is documented at
https://shopify.github.io/ghostferry/master/technicaloverview.html

Development Setup
-----------------

Install:

- Have Docker installed
- Clone the repo
- `docker-compose up -d mysql-1 mysql-2`

Run tests:

- `make test`

Test copydb:

- `make copydb && ghostferry-copydb -verbose examples/copydb/conf.json`
- For a more detailed tutorial, see the
  [documentation](https://shopify.github.io/ghostferry).

Ruby Integration Tests
----

Kindly take note of following options:
*   `DEBUG=1`: To see more detailed debug output by `Ghostferry`
*   `TESTOPTS=...`: As detailed under https://docs.ruby-lang.org/en/2.1.0/Rake/TestTask.html

Example:
`bundle exec rake test DEBUG=1 TESTOPTS="-v --name=TrivialIntegrationTests#test_logged_query_omits_columns"`
