# Deploying to Kubernetes (Helm)

Chart `deploy/helm/karhutlasql`: a 1-replica Deployment (DuckDB is
single-writer, `Recreate` strategy) + a PVC at `/data`, a Service (31337
Flight SQL, 9090 admin), readiness/liveness via `GET /healthz`, non-root
65532, read-only root FS.

```bash
# secrets from files, not argv
kubectl create secret generic karhutlasql-auth \
  --from-file=password=./password --from-file=jwt-secret=./jwt-secret
kubectl create secret tls karhutlasql-tls --cert=server.crt --key=server.key

helm install kq deploy/helm/karhutlasql \
  --set auth.existingSecret=karhutlasql-auth \
  --set tls.existingSecret=karhutlasql-tls
```

| Value | Default | Description |
|---|---|---|
| `auth.existingSecret` | — (required) | key `password` (plaintext/bcrypt), optional `jwt-secret` (without it tokens are invalidated on restart) |
| `auth.username` | `admin` | |
| `tls.existingSecret` | `""` | empty = in-memory self-signed (dev) |
| `tls.initImage` | `busybox:1.37` | init container that copies the key to an emptyDir with mode 0600 (the server rejects keys readable by group; a Secret mounted with `fsGroup` is 0440) |
| `queryTimeout` | `300` | seconds |
| `fourEyes` | `true` | a new policy/classification ConfigMap (restart) stays pending until 2 admins approve it ([policy](policy.md#four-eyes-g-11)); state on the PVC |
| `archive.s3.*`, `archive.existingSecret`, `archive.retention` | empty | access-history & report archive in an object-lock bucket; Secret keys `access-key`, `secret-key` |
| `reports`, `reportSigning.existingSecret` | empty | contents of reports.yaml; Secret key `report.key` (copied to an emptyDir with mode 0600) |
| `dq.maxAge` | `26h` | report-run DQ gate (GX validation per source table); `"0"` = off |
| `unityCatalog.endpoint`, `.syncInterval`, `.advertisedEndpoint`, `.existingSecret` | empty / `15m` | metadata sync to Unity Catalog OSS; Secret key `uc-token` (optional) |
| `allowedDirectories` | `[]` | paths/URL prefixes DuckDB may access (G-07) |
| `extraArgs` | `[]` | extra server flags |
| `persistence.size` / `storageClass` | `10Gi` / default | |
| `image.repository` / `tag` | `karhutlasql` / `dev` | |

Local test (kind): `make docker && kind load docker-image karhutlasql:dev`, then
`kubectl port-forward svc/kq-karhutlasql 31337` and
`karhutlasql-client --tls-ca server.crt`.
