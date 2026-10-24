# Data catalog: Unity Catalog OSS sync (K-01…K-04)

KarhutlaSQL pushes catalog/schema/table/column metadata to Unity Catalog
OSS — **one way**. UC is only a mirror for data discovery; permissions,
masking, and RLS are still decided by the server from
`policy.yaml`/`classification.yaml`, and values in UC are never read back.

```
--uc-endpoint http://unity-catalog:8080   # empty = off
--uc-token-env KARHUTLASQL_UC_TOKEN       # optional Bearer
--uc-sync-interval 15m
--uc-advertised-endpoint grpc+tls://karhutlasql.corp:31337   # default grpc[+tls]://host:port
```

## What is synced

- Tables (not views/temp) with their columns: type (DuckDB → the Spark type
  UC expects; types without an equivalent such as `LIST`/`STRUCT` →
  `string`, the DuckDB type text is kept in `type_text`), nullable, comment.
  Names are lowercased.
- Table properties:

  | Property | Contents |
  |---|---|
  | `karhutla.classification.<column>` | G-03 tag |
  | `karhutla.dq.status` | `passed` / `failed` / `none` (latest GX validation) |
  | `karhutla.dq.validation_id`, `karhutla.dq.validated_at` | latest validation |
  | `karhutla.endpoint` | Flight SQL endpoint for consumers |
  | `karhutla.managed` | `true` — object owned by KarhutlaSQL |

- Tables are registered as `EXTERNAL` with 
  `storage_location = file:///karhutlasql-unreadable/<catalog>/<schema>/<table>`
  (a placeholder that does not exist). The UC deployment **must not** hold credentials for
  KarhutlaSQL's buckets, so credential vending returns nothing (ADR 13).

## When & how

- Triggers: at startup, every `--uc-sync-interval`, after a successful DDL
  (`CREATE`/`DROP`/`ALTER`/`ATTACH`/`DETACH`), after `dq_publish`, and after a
  new policy/classification becomes active (four-eyes). Changes usually show
  up in UC within seconds.
- A full diff every round: what is missing is created, what differs is
  deleted and recreated (UC OSS 0.7 has no `PATCH /tables`; `table_id`
  changes — grant metadata access at the schema/catalog level), what no
  longer exists is deleted. Manual changes in UC are overwritten.
- Only catalogs created by KarhutlaSQL (`karhutla.managed=true`) are
  touched; another UC catalog with the same name → sync error, not
  overwritten.
- UC unavailable → queries keep running; sync is retried with backoff
  (1 second … interval). Every operation is recorded in the `catalog_sync`
  audit event (`object`, `detail` = create/update/delete/list, `status`);
  failures increment `karhutla_catalog_sync_errors_total`.

## Dev

```
docker compose -f deploy/compose/compose.yaml --profile uc up -d unity-catalog
karhutlasql-server … --uc-endpoint http://127.0.0.1:8080
```

## Limits

Views, and schemas/catalogs without tables, are not synced; there is no
owner property yet (the policy has no notion of an owner).
