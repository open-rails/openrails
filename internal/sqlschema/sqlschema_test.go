package sqlschema

import (
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/config"
	"github.com/stretchr/testify/require"
)

func TestRewriteRule(t *testing.T) {
	const in = `-- billing.payments stays documentation
/* outer billing.x /* nested billing */ comment */
CREATE SCHEMA IF NOT EXISTS billing;
GRANT USAGE ON SCHEMA billing TO r;
CREATE TABLE "billing".payment_methods (driver text CHECK (driver IN ('openrails', 'billing')), note text DEFAULT 'billing.payment_methods');
CREATE FUNCTION billing.sample() RETURNS text SET search_path TO 'pg_catalog', 'billing' LANGUAGE plpgsql AS $body$
BEGIN
 PERFORM pg_advisory_xact_lock(hashtextextended('openrails.default_payment_method:' || 'x', 0));
 PERFORM 'billing.payment_methods'::regclass, to_regclass('billing.payments'), 'billing'::regnamespace;
 PERFORM 1 FROM billing.payment_methods;
 SET LOCAL search_path = billing, public;
 RETURN 'billing';
END
$body$;
SELECT x::billing.payment_status FROM BILLING.payments WHERE note = $1;`
	const want = `-- billing.payments stays documentation
/* outer billing.x /* nested billing */ comment */
CREATE SCHEMA IF NOT EXISTS shop;
GRANT USAGE ON SCHEMA shop TO r;
CREATE TABLE "shop".payment_methods (driver text CHECK (driver IN ('openrails', 'billing')), note text DEFAULT 'billing.payment_methods');
CREATE FUNCTION shop.sample() RETURNS text SET search_path TO 'pg_catalog', 'shop' LANGUAGE plpgsql AS $body$
BEGIN
 PERFORM pg_advisory_xact_lock(hashtextextended('openrails.default_payment_method:' || 'x', 0));
 PERFORM 'shop.payment_methods'::regclass, to_regclass('shop.payments'), 'shop'::regnamespace;
 PERFORM 1 FROM shop.payment_methods;
 SET LOCAL search_path = shop, public;
 RETURN 'billing';
END
$body$;
SELECT x::shop.payment_status FROM shop.payments WHERE note = $1;`
	got, err := Rewrite(in, "shop")
	require.NoError(t, err)
	require.Equal(t, want, got)
	for _, schema := range []string{"", config.DefaultSchema} {
		got, err := Rewrite(in, schema)
		require.NoError(t, err)
		require.Equal(t, in, got, "the default schema runs SQL verbatim")
	}

	for _, data := range []string{"SELECT $$FROM billing.payment_methods$$, $tag$billing$tag$;", `SELECT 'it''s billing.x', E'it\'s billing.x', "not billing";`} {
		got, err := Rewrite(data, "shop")
		require.NoError(t, err)
		require.Equal(t, data, got)
	}
	for _, bad := range []string{"SELECT 'unfinished", "SELECT \"unfinished", "/* outer /* inner */", "DO $body$BEGIN"} {
		_, err := Rewrite(bad, "shop")
		require.Error(t, err, bad)
	}
}

func TestRelocatorCachesPerStatement(t *testing.T) {
	require.Nil(t, New(""))
	require.Nil(t, New(config.DefaultSchema))
	var def *Relocator
	require.Equal(t, config.DefaultSchema, def.Schema())
	got, err := def.SQL("SELECT 1 FROM billing.x")
	require.NoError(t, err)
	require.Equal(t, "SELECT 1 FROM billing.x", got)

	r := New("shop")
	require.Equal(t, "shop", r.Schema())
	for range 2 {
		got, err := r.SQL("SELECT 1 FROM billing.x")
		require.NoError(t, err)
		require.Equal(t, "SELECT 1 FROM shop.x", got)
		_, err = r.SQL("SELECT 'unfinished")
		require.Error(t, err)
	}
	require.EqualValues(t, 2, r.cached.Load())
}

// Every sqlc query and the baseline relocate completely: no authored qualifier
// is left in code, and every literal and comment is byte-identical except the
// reg* and search_path ones that name the schema.
func TestEveryAuthoredStatementRelocates(t *testing.T) {
	statements := map[string]string{}
	gen, err := filepath.Glob("../db/gen/*.sql.go")
	require.NoError(t, err)
	require.NotEmpty(t, gen)
	fset := gotoken.NewFileSet()
	for _, path := range gen {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			if d, ok := decl.(*ast.GenDecl); ok && d.Tok == gotoken.CONST {
				for _, spec := range d.Specs {
					for i, v := range spec.(*ast.ValueSpec).Values {
						if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == gotoken.STRING {
							sql, err := strconv.Unquote(lit.Value)
							require.NoError(t, err)
							statements[path+":"+spec.(*ast.ValueSpec).Names[i].Name] = sql
						}
					}
				}
			}
		}
	}
	baselines, err := filepath.Glob("../migrate/postgres/*.sql")
	require.NoError(t, err)
	require.NotEmpty(t, baselines)
	for _, path := range baselines {
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		statements[path] = string(b)
	}
	require.Greater(t, len(statements), 300)

	const schema = "tenant_custom"
	for name, sql := range statements {
		out, err := Rewrite(sql, schema)
		require.NoError(t, err, name)
		require.NoError(t, onlySchemaMoved(sql, out, schema), name)
	}
}

func onlySchemaMoved(in, out, schema string) error {
	a, err := tokenize(in)
	if err != nil {
		return err
	}
	b, err := tokenize(out)
	if err != nil {
		return err
	}
	if len(a) != len(b) {
		return fmt.Errorf("token count %d -> %d", len(a), len(b))
	}
	from := config.DefaultSchema
	endA, endB := 0, 0
	for i := range a {
		if in[endA:a[i].start] != out[endB:b[i].start] {
			return fmt.Errorf("comment or spacing changed before %q", in[a[i].start:a[i].end])
		}
		x, y := in[a[i].start:a[i].end], out[b[i].start:b[i].end]
		endA, endB = a[i].end, b[i].end
		switch {
		case a[i].kind == identifier && (strings.EqualFold(x, from) || x == `"`+from+`"`):
			if y != schema && y != `"`+schema+`"` {
				return fmt.Errorf("authored schema %q left in code (got %q)", x, y)
			}
		case x == y:
		case a[i].kind == literal && strings.HasPrefix(x, "$"):
			d := x[:strings.IndexByte(x[1:], '$')+2]
			if err := onlySchemaMoved(x[len(d):len(x)-len(d)], y[len(d):len(y)-len(d)], schema); err != nil {
				return fmt.Errorf("function body: %w", err)
			}
		case a[i].kind == literal && y == strings.Replace(x, from, schema, 1) &&
			(x == "'"+from+"'" || strings.HasPrefix(x, "'"+from+".")):
		default:
			return fmt.Errorf("token changed: %q -> %q", x, y)
		}
	}
	if in[endA:] != out[endB:] {
		return fmt.Errorf("trailing comment changed")
	}
	return nil
}
