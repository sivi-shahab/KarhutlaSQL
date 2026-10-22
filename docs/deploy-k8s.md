# Deploy ke Kubernetes (Helm)

Chart `deploy/helm/karhutlasql`: Deployment 1 replica (DuckDB single-writer,
strategi `Recreate`) + PVC `/data`, Service (31337 Flight SQL, 9090 admin),
readiness/liveness `GET /healthz`, non-root 65532, root FS read-only.

```bash
# secret dari file, bukan argv
kubectl create secret generic karhutlasql-auth \
  --from-file=password=./password --from-file=jwt-secret=./jwt-secret
kubectl create secret tls karhutlasql-tls --cert=server.crt --key=server.key

helm install kq deploy/helm/karhutlasql \
  --set auth.existingSecret=karhutlasql-auth \
  --set tls.existingSecret=karhutlasql-tls
```

| Value | Default | Keterangan |
|---|---|---|
| `auth.existingSecret` | — (wajib) | key `password` (plaintext/bcrypt), `jwt-secret` opsional (tanpa ini token hangus saat restart) |
| `auth.username` | `admin` | |
| `tls.existingSecret` | `""` | kosong = self-signed di memori (dev) |
| `tls.initImage` | `busybox:1.37` | init container yang menyalin kunci ke emptyDir 0600 (server menolak kunci yang bisa dibaca group; mount Secret dengan `fsGroup` = 0440) |
| `queryTimeout` | `300` | detik |
| `extraArgs` | `[]` | flag server tambahan |
| `persistence.size` / `storageClass` | `10Gi` / default | |
| `image.repository` / `tag` | `karhutlasql` / `dev` | |

Uji lokal (kind): `make docker && kind load docker-image karhutlasql:dev`, lalu
`kubectl port-forward svc/kq-karhutlasql 31337` dan
`karhutlasql-client --tls-ca server.crt`.
