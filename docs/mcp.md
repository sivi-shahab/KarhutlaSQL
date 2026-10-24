# Server MCP untuk agen AI

KarhutlaSQL menyediakan server [Model Context Protocol](https://modelcontextprotocol.io)
agar agen AI (Claude, Copilot, agen internal) bisa menjelajah skema dan
menjalankan query **di bawah governance yang sama** dengan klien lain.
Setiap tool call menjadi statement Flight SQL dengan token pemanggil, jadi
izin, masking, RLS, kuota, result cache, access history, dan audit berlaku
apa adanya (ADR 37). Audit `statement_*` membawa `"channel": "mcp"`.

## Profil `ai_sandbox` untuk semua panggilan MCP

Batas role `ai_sandbox` di policy (`max_rows`, `max_bytes`, `timeout`,
`resource_group`, selalu SELECT-only) diterapkan **di atas** role pemanggil
untuk setiap statement lewat MCP — juga untuk admin. Yang ikut hanya
batasnya; izin catalog `ai_sandbox` tidak ditambahkan. Tanpa role
`ai_sandbox` di policy: SELECT-only, 500 baris, 60 detik. Izin baca dan
unmask tetap dari role pemanggil, jadi beri agen identitas sendiri (mis.
service account Keycloak di grup `ai-agent`) bila agen tidak boleh melihat
data yang dilihat pemiliknya.

Penandaan kanal hanya bisa **memperketat**: klien yang mengirim header
`x-karhutla-channel: mcp` sendiri hanya membatasi dirinya.

## Tools & resources

| Tool | Fungsi |
|---|---|
| `list_catalogs` | catalog yang boleh dibaca |
| `list_tables` `{catalog?, schema?}` | tabel yang boleh dibaca |
| `describe_table` `{catalog, schema, table}` | kolom & tipe |
| `run_query` `{sql}` | satu SELECT; hasil JSON (`columns`, `rows`, `row_count`, `truncated`), dibatasi `--mcp-max-rows` (500) dan 1 MiB |
| `list_verified_queries` | pertanyaan bisnis → SQL terverifikasi |
| `run_verified_query` `{name}` | jalankan verified query (sebagai pemanggil) |

Resource: `karhutla://semantic-model` (YAML) dan `karhutla://verified-queries` (JSON).
Semua tool read-only; tidak ada tool tulis/DDL.

## Verified queries & semantic model

`--verified-queries-file verified-queries.yaml` (Helm: `verifiedQueries`):

```yaml
semantic_model: |
  tables:
    warehouse.main.nasabah:
      description: nasabah aktif per cabang
      dimensions: [branch_code]
      metrics: { jumlah_nasabah: "count(*)", total_saldo: "sum(saldo)" }
verified_queries:
  nasabah_per_cabang:
    question: Berapa jumlah nasabah per cabang?
    sql: SELECT branch_code, count(*) AS jumlah FROM warehouse.main.nasabah GROUP BY 1 ORDER BY 1
    verified_by: dian@bankmega.com
    verified_at: 2026-10-01T00:00:00Z
```

Server menolak start bila salah satu SQL bukan satu SELECT yang valid.
Klien SQL biasa juga bisa membaca: `SELECT * FROM karhutla_verified_queries()`,
`SELECT * FROM karhutla_semantic_model()`. Perubahan file berlaku setelah
restart; tinjau lewat review Git (bukan four-eyes server).

## Transport HTTP (di server)

```bash
karhutlasql-server ... --mcp-port 8443 --mcp-resource-url https://kq.bank.local:8443/mcp
```

- Endpoint `/mcp` (Streamable HTTP), TLS sama dengan gRPC. Setiap request
  wajib `Authorization: Bearer <token>` (token login lokal atau OIDC dengan
  audience KarhutlaSQL); tanpa/token salah → `401` dengan
  `WWW-Authenticate: Bearer resource_metadata=…`.
- `--mcp-resource-url` mengaktifkan metadata OAuth (RFC 9728) di
  `/.well-known/oauth-protected-resource` yang menunjuk issuer
  `--oidc-issuer`, sehingga klien MCP bisa login sendiri ke Keycloak.
- Sesi MCP terikat ke user token pertama dan ditutup setelah 30 menit idle;
  sesi yang di-kill (`kill_session`) ditolak.
- Server memanggil dirinya lewat gRPC in-process (tanpa jaringan), jadi rate
  limit, sesi, dan audit sama dengan klien Flight SQL.

## Transport stdio (di klien)

Untuk agen lokal (Claude Desktop/Code), `karhutlasql-client --mcp` menjadi
server MCP stdio yang terhubung ke server dengan kredensial pengguna:

```json
{
  "mcpServers": {
    "karhutlasql": {
      "command": "karhutlasql-client",
      "args": ["--mcp", "--host", "kq.bank.local", "--tls-ca", "/etc/ssl/bank-ca.pem",
               "--oidc-issuer", "https://sso.bank.local/realms/data"]
    }
  }
}
```

Login SSO dulu sekali (`karhutlasql-client --oidc-issuer … --oidc-login`);
token tersimpan dipakai dan di-refresh. Tanpa SSO, password dari env
`KARHUTLASQL_PASSWORD`.
