package flightsql

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/apache/arrow-go/v18/arrow/flight"
	fsql "github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/authz"
	"github.com/bankmega/karhutlasql/internal/engine"
)

// DoPutCommandStatementIngest bulk ingest Flight SQL (ADBC adbc_ingest,
// Fase 8d): stream Arrow ditulis ke satu tabel dalam satu transaksi.
// Butuh izin write pada tabel target (role SELECT-only / kanal MCP ditolak);
// antrean, kuota, audit, dan access history sama dengan statement tulis.
func (s *Server) DoPutCommandStatementIngest(ctx context.Context, cmd fsql.StatementIngest, rdr flight.MessageReader) (int64, error) {
	if len(cmd.GetTransactionId()) > 0 {
		return 0, status.Error(codes.Unimplemented, "transactions are not supported")
	}
	if cmd.GetTemporary() {
		return 0, status.Error(codes.InvalidArgument, "temporary tables are not supported (each statement uses its own connection)")
	}
	if cmd.GetTable() == "" {
		return 0, status.Error(codes.InvalidArgument, "ingest needs a table name")
	}
	t := engine.IngestTarget{Catalog: cmd.GetCatalog(), Schema: cmd.GetSchema(), Table: cmd.GetTable(),
		IfNotExists: engine.IngestCreate, IfExists: engine.IngestFail}
	if o := cmd.GetTableDefinitionOptions(); o != nil {
		if o.IfNotExist == fsql.TableDefinitionOptionsTableNotExistOptionFail {
			t.IfNotExists = engine.IngestFail
		}
		switch o.IfExists {
		case fsql.TableDefinitionOptionsTableExistsOptionAppend:
			t.IfExists = engine.IngestAppend
		case fsql.TableDefinitionOptionsTableExistsOptionReplace:
			t.IfExists = engine.IngestReplace
		}
	}
	a, err := s.eng.Analyze(ctx, "SELECT 1")
	if err != nil {
		return 0, toStatus(ctx, err)
	}
	if t.Catalog == "" {
		t.Catalog = a.DefaultCatalog
	}
	if t.Schema == "" {
		t.Schema = "main"
	}
	object := strings.ToLower(t.Catalog + "." + t.Schema + "." + t.Table)
	desc := fmt.Sprintf("INGEST INTO %s (if_not_exists=%d, if_exists=%d)", object, t.IfNotExists, t.IfExists)
	qctx, _, end, err := s.beginWith(ctx, "update", desc, func(ctx context.Context) (plan, error) { return s.authorizeIngest(ctx, object, desc, t) })
	if err != nil {
		return 0, err
	}
	n, err := s.eng.Ingest(qctx, t, rdr)
	if errors.Is(err, engine.ErrIngestTarget) {
		err = status.Error(codes.FailedPrecondition, err.Error())
	} else if err != nil {
		err = toStatus(qctx, err)
	}
	end(n, 0, err)
	return n, err
}

// authorizeIngest izin write pada objek target (dan profil yang
// mengizinkan tulis), tanpa SQL untuk dianalisis.
func (s *Server) authorizeIngest(ctx context.Context, object, desc string, t engine.IngestTarget) (plan, error) {
	id, _ := auth.FromContext(ctx)
	p := s.opts.Policy.Policy()
	pl := plan{sql: desc, limits: p.LimitsFor(id.Roles), writes: []string{object}}
	if Channel(ctx) == auth.ChannelMCP {
		pl.limits = pl.limits.Tighten(p.AgentLimits())
	}
	kind := "INSERT"
	if t.IfNotExists == engine.IngestCreate || t.IfExists == engine.IngestReplace {
		kind = "CREATE"
	}
	pl.final = engine.Analysis{Kind: kind, Catalogs: []string{strings.ToLower(t.Catalog)}}
	var err error
	if pl.limits.SelectOnly {
		err = errors.New("role is SELECT-only")
	} else {
		err = p.Check(id.Roles, []authz.Need{{Object: object, Action: authz.Write}})
	}
	if err != nil {
		return plan{}, s.deny(ctx, kind, desc, err)
	}
	return pl, nil
}
