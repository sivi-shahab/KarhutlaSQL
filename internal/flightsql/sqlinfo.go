package flightsql

import (
	"context"
	"math"
	"runtime/debug"
	"strings"
	"time"

	fsql "github.com/apache/arrow-go/v18/arrow/flight/flightsql"

	"github.com/bankmega/karhutlasql/internal/engine"
	"github.com/bankmega/karhutlasql/internal/version"
)

// sqlInfo nilai GetSqlInfo (Fase 8d): kemampuan server & dialek DuckDB untuk
// klien JDBC/ADBC/BI. Transaksi Flight SQL tidak didukung (setiap statement
// koneksi sendiri); bulk ingest didukung tanpa transaksi.
func sqlInfo(eng engine.Engine, timeout time.Duration) map[fsql.SqlInfo]any {
	info := map[fsql.SqlInfo]any{
		fsql.SqlInfoFlightSqlServerName:                        "KarhutlaSQL",
		fsql.SqlInfoFlightSqlServerVersion:                     version.Version,
		fsql.SqlInfoFlightSqlServerArrowVersion:                arrowVersion,
		fsql.SqlInfoFlightSqlServerReadOnly:                    false,
		fsql.SqlInfoFlightSqlServerSql:                         true,
		fsql.SqlInfoFlightSqlServerSubstrait:                   false,
		fsql.SqlInfoFlightSqlServerTransaction:                 int32(fsql.SqlTransactionNone),
		fsql.SqlInfoFlightSqlServerCancel:                      false,                                          // pembatalan = putus stream / kill_session
		fsql.SqlInfoFlightSqlServerStatementTimeout:            int32(min(timeout/time.Second, math.MaxInt32)), //nolint:gosec // dibatasi min
		fsql.SqlInfoFlightSqlServerBulkIngestion:               true,
		fsql.SqlInfoFlightSqlServerIngestTransactionsSupported: false,
		fsql.SqlInfoDDLCatalog:                                 false, // ATTACH = operasi admin, bukan CREATE CATALOG
		fsql.SqlInfoDDLSchema:                                  true,
		fsql.SqlInfoDDLTable:                                   true,
		fsql.SqlInfoIdentifierCase:                             int32(fsql.SqlCaseSensitivityCaseInsensitive),
		fsql.SqlInfoIdentifierQuoteChar:                        `"`,
		fsql.SqlInfoQuotedIdentifierCase:                       int32(fsql.SqlCaseSensitivityCaseInsensitive),
		fsql.SqlInfoAllTablesAreASelectable:                    false, // izin per tabel (G-02)
		fsql.SqlInfoNullOrdering:                               int32(fsql.SqlNullOrderingSortAtEnd),
		fsql.SqlInfoSearchStringEscape:                         `\`,
		fsql.SqlInfoExtraNameChars:                             "",
		fsql.SqlInfoSupportsColumnAliasing:                     true,
		fsql.SqlInfoNullPlusNullIsNull:                         true,
		fsql.SqlInfoSupportsTableCorrelationNames:              true,
		fsql.SqlInfoSupportsDifferentTableCorrelationNames:     false,
		fsql.SqlInfoSupportsExpressionsInOrderBy:               true,
		fsql.SqlInfoSupportsOrderByUnrelated:                   true,
		fsql.SqlInfoSupportedGroupBy:                           int32(0b11), // unrelated | beyond select
		fsql.SqlInfoSupportsLikeEscapeClause:                   true,
		fsql.SqlInfoSupportsNonNullableColumns:                 true,
		fsql.SqlInfoSupportedGrammar:                           int32(0b111), // minimum | core | extended
		fsql.SqlInfoANSI92SupportedLevel:                       int32(0b011), // entry | intermediate
		fsql.SqlInfoSupportsIntegrityEnhancementFacility:       true,
		fsql.SqlInfoOuterJoinsSupportLevel:                     int32(2), // full outer joins
		fsql.SqlInfoSchemaTerm:                                 "schema",
		fsql.SqlInfoProcedureTerm:                              "macro",
		fsql.SqlInfoCatalogTerm:                                "catalog",
		fsql.SqlInfoCatalogAtStart:                             true,
		fsql.SqlInfoSchemasSupportedActions:                    int32(0),
		fsql.SqlInfoCatalogsSupportedActions:                   int32(0),
		fsql.SqlInfoSupportedPositionedCommands:                int32(0),
		fsql.SqlInfoSelectForUpdateSupported:                   false,
		fsql.SqlInfoStoredProceduresSupported:                  false,
		fsql.SqlInfoSupportedSubqueries:                        int32(0b1111), // comparisons | exists | in | quantified
		fsql.SqlInfoCorrelatedSubqueriesSupported:              true,
		fsql.SqlInfoSupportedUnions:                            int32(0b11), // union | union all
		fsql.SqlInfoMaxBinaryLiteralLen:                        int64(0),    // 0 = tanpa batas
		fsql.SqlInfoMaxCharLiteralLen:                          int64(0),
		fsql.SqlInfoMaxColumnNameLen:                           int64(0),
		fsql.SqlInfoMaxColumnsInGroupBy:                        int64(0),
		fsql.SqlInfoMaxColumnsInIndex:                          int64(0),
		fsql.SqlInfoMaxColumnsInOrderBy:                        int64(0),
		fsql.SqlInfoMaxColumnsInSelect:                         int64(0),
		fsql.SqlInfoMaxColumnsInTable:                          int64(0),
		fsql.SqlInfoMaxConnections:                             int64(0),
		fsql.SqlInfoMaxStatementLen:                            int64(0),
		fsql.SqlInfoMaxStatements:                              int64(1), // satu statement per permintaan
		fsql.SqlInfoMaxTableNameLen:                            int64(0),
		fsql.SqlInfoMaxTablesInSelect:                          int64(0),
		fsql.SqlInfoDefaultTransactionIsolation:                int32(0),
		fsql.SqlInfoTransactionsSupported:                      false,
		fsql.SqlInfoBatchUpdatesSupported:                      true,
		fsql.SqlInfoSavePointsSupported:                        false,
		fsql.SqlInfoNamedParametersSupported:                   false,
		fsql.SqlInfoStoredFunctionsUsingCallSyntaxSupported:    false,
	}
	if kw := firstColumn(eng, "SELECT keyword_name FROM duckdb_keywords() ORDER BY 1"); len(kw) > 0 {
		info[fsql.SqlInfoKeywords] = kw
	}
	if fn := firstColumn(eng, "SELECT DISTINCT upper(function_name) FROM duckdb_functions() WHERE function_type = 'scalar' AND NOT internal ORDER BY 1"); len(fn) > 0 {
		info[fsql.SqlInfoSystemFunctions] = fn
	}
	return info
}

// firstColumn kolom pertama hasil query (metadata engine; gagal = kosong).
func firstColumn(eng engine.Engine, q string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rdr, err := eng.Query(ctx, q)
	if err != nil {
		return nil
	}
	defer rdr.Release()
	var out []string
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for i := range int(rec.NumRows()) {
			out = append(out, strings.Clone(rec.Column(0).ValueStr(i)))
		}
	}
	return out
}

// arrowVersion versi modul arrow-go di binary ("" bila tidak diketahui).
var arrowVersion = func() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "github.com/apache/arrow-go/v18" {
				return d.Version
			}
		}
	}
	return ""
}()
