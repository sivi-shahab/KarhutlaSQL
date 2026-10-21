// Smoke test driver Arrow Flight SQL JDBC (yang dipakai DBeaver): metadata + TPC-H Q1.
// java --add-opens=java.base/java.nio=ALL-UNNAMED -cp flight-sql-jdbc-driver-<v>.jar tests/jdbc/JdbcSmoke.java <port>
import java.sql.Connection;
import java.sql.DatabaseMetaData;
import java.sql.DriverManager;
import java.sql.ResultSet;
import java.sql.Statement;

public class JdbcSmoke {
    public static void main(String[] args) throws Exception {
        String url = "jdbc:arrow-flight-sql://127.0.0.1:" + args[0] + "/?useEncryption=false";
        try (Connection c = DriverManager.getConnection(url)) {
            DatabaseMetaData md = c.getMetaData();
            System.out.println("server: " + md.getDatabaseProductName() + " " + md.getDatabaseProductVersion());
            check(count(md.getCatalogs()) > 0, "getCatalogs");
            check(count(md.getSchemas()) > 0, "getSchemas");
            check(count(md.getTables(null, null, "lineitem", null)) == 1, "getTables(lineitem)");
            int cols = count(md.getColumns(null, null, "lineitem", null));
            check(cols == 16, "getColumns(lineitem)=" + cols);
            try (Statement st = c.createStatement();
                 ResultSet rs = st.executeQuery(new String(java.nio.file.Files.readAllBytes(java.nio.file.Path.of("tests/jdbc/tpch_q1.sql"))))) {
                int rows = 0;
                while (rs.next()) {
                    rows++;
                    System.out.println("  q1 " + rs.getString(1) + rs.getString(2) + " count_order=" + rs.getLong("count_order"));
                }
                check(rows == 4, "tpch q1 rows=" + rows);
            }
            try (Statement st = c.createStatement()) {
                st.executeUpdate("CREATE TABLE IF NOT EXISTS jdbc_t (i INTEGER)");
                int n = st.executeUpdate("INSERT INTO jdbc_t VALUES (1), (2)");
                check(n == 2, "executeUpdate=" + n);
            }
        }
        System.out.println("JDBC OK");
    }

    static int count(ResultSet rs) throws Exception {
        int n = 0;
        try (rs) {
            while (rs.next()) n++;
        }
        return n;
    }

    static void check(boolean ok, String what) {
        System.out.println((ok ? "  ok   " : "  FAIL ") + what);
        if (!ok) throw new AssertionError(what);
    }
}
