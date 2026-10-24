# 3. JDBC, DBeaver & applications

**Goal:** connect JVM tools such as DBeaver, and your own Java or Go code, to KarhutlaSQL.

**You will learn**

- the Arrow Flight SQL JDBC URL and its options
- how to add KarhutlaSQL as a DBeaver connection
- minimal Java and Go programs that run a query
- what to look for in other tools

**Prerequisites**

- A running server (see [tutorial 1](/tutorials/01-first-query)), plus the `nasabah` table
- For JDBC: Java 11+ and the **Apache Arrow Flight SQL JDBC driver** jar
  (`flight-sql-jdbc-driver-<version>.jar`, published by Apache Arrow on Maven Central;
  the smoke test in this repository uses 19.0.0)

## Step 1 — The JDBC URL

```
jdbc:arrow-flight-sql://<host>:31337/?useEncryption=true
```

| Option | Use |
|---|---|
| `useEncryption=true` | TLS. Use `false` only for a plaintext dev server. |
| `disableCertificateVerification=true` | dev only: accept the certificate from `--tls-self-signed` |
| `trustStore=…&trustStorePassword=…` | trust your internal CA in production |
| `token=<access_token>` | log in with an OIDC token instead of a username and password |

The username and password are passed as normal JDBC credentials. The driver
uses Flight Basic auth, and the server returns a session token.

## Step 2 — DBeaver

1. **Database → Driver Manager → New**.
   - Driver name: `KarhutlaSQL (Arrow Flight SQL)`
   - Class name: `org.apache.arrow.driver.jdbc.ArrowFlightJdbcDriver`
   - URL template: `jdbc:arrow-flight-sql://{host}:{port}/`
   - Default port: `31337`
   - Libraries tab: add `flight-sql-jdbc-driver-<version>.jar`
2. **New Database Connection** → choose the driver. Set host and port, user
   `admin`, and the password.
3. **Driver properties** tab:
   - set `useEncryption` to `true`;
   - for a dev server with a self-signed certificate, also set
     `disableCertificateVerification` to `true`.
4. Recent DBeaver versions running on Java 17+ need this line in `dbeaver.ini`
   (after `-vmargs`): `--add-opens=java.base/java.nio=ALL-UNNAMED`.
   Arrow's memory module needs it.
5. **Test connection**, then open the navigator. You will see only the
   catalogs, schemas, and tables your role is allowed to read. In this case
   that is `demo → main → nasabah`.

Everything DBeaver runs goes through the same governance as the CLI:
masking, row filters, query history, and audit.

## Step 3 — Java

```java
import java.sql.*;

public class Hello {
    public static void main(String[] args) throws Exception {
        String url = "jdbc:arrow-flight-sql://127.0.0.1:31337/?useEncryption=true&disableCertificateVerification=true";
        try (Connection c = DriverManager.getConnection(url, "admin", System.getenv("KARHUTLASQL_PASSWORD"));
             Statement st = c.createStatement();
             ResultSet rs = st.executeQuery("SELECT branch_code, saldo FROM nasabah ORDER BY 1")) {
            while (rs.next()) {
                System.out.println(rs.getString(1) + " " + rs.getBigDecimal(2));
            }
        }
    }
}
```

```bash
java --add-opens=java.base/java.nio=ALL-UNNAMED -cp flight-sql-jdbc-driver-19.0.0.jar Hello.java
```

`DatabaseMetaData` (`getCatalogs`, `getTables`, `getColumns`) and
`executeUpdate` also work. The repository's smoke test,
`tests/jdbc/JdbcSmoke.java`, exercises them against TPC-H.

## Step 4 — Go with `pkg/client`

The CLI is built on a small Go library, `pkg/client`. It handles TLS/mTLS,
login, SSO tokens, and execution, and returns an Arrow `flight.Reader`.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/bankmega/karhutlasql/pkg/client"
)

func main() {
	ctx := context.Background()
	c, err := client.Dial(ctx, client.Config{
		Addr:       "127.0.0.1:31337",
		SkipVerify: true, // dev only; production: CAFile: "ca.crt"
		Username:   "admin",
		Password:   os.Getenv("KARHUTLASQL_PASSWORD"),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	rdr, err := c.Query(ctx, "SELECT kota, count(*) AS n FROM kota_demo GROUP BY 1 ORDER BY 1")
	if err != nil {
		log.Fatal(err)
	}
	defer rdr.Release()
	for rdr.Next() {
		rec := rdr.Record()
		for i := 0; i < int(rec.NumRows()); i++ {
			fmt.Println(rec.Column(0).ValueStr(i), rec.Column(1).ValueStr(i))
		}
	}
	if err := rdr.Err(); err != nil {
		log.Fatal(err)
	}
}
```

```
Bandung 505
Jakarta 505
```

`kota_demo` is the table created in [tutorial 2](/tutorials/02-python).

The Go module path is `github.com/bankmega/karhutlasql`. Build inside a
checkout of this repository, or point your `go.mod` at a local checkout
(`replace github.com/bankmega/karhutlasql => ../KarhutlaSQL`).

Other `Config` fields:

- `CAFile` — trust your internal CA.
- `CertFile` and `KeyFile` — client certificate for mTLS.
- `TokenSource` — an `oauth2.TokenSource` for SSO, for example from
  `pkg/client/oauth`. The token is refreshed automatically.

## Other tools

KarhutlaSQL speaks standard **Arrow Flight SQL**, so it works with any tool
that has a Flight SQL JDBC or ADBC driver. That includes JVM BI tools through
the JDBC driver above, ADBC from Python/R/Go/C, and Flight SQL data sources
in dashboarding tools. Connection settings are always the same:

- host and port `31337`;
- TLS on;
- username and password, or a Bearer token.

Server capabilities are advertised through `GetSqlInfo`:

- server name `KarhutlaSQL`;
- DuckDB SQL dialect;
- bulk ingest supported;
- transactions not supported.

## What just happened

- The JDBC driver and the Go client both log in with Flight Basic auth (or a
  Bearer token), then send each query as Flight SQL `GetFlightInfo` + `DoGet`.
- Metadata calls (`getTables` and the DBeaver navigator) are answered by the
  server and filtered to what your role can read. Even the list of objects is
  governed.

## Next

[4. Masking & row-level security →](/tutorials/04-masking-rls)
