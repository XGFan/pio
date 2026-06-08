# One-time migration: `webshare*` → `pio*` (k8s objects + live data)

The project was renamed to **pio**. The k8s object names and the data PVC were
renamed too (`webshare` → `pio`, `webshare-data` → `pio-data`, `webshare-proxy`
→ `pio-proxy`). This is **not** an in-place change: `kubectl apply` of the new
manifest would create *parallel* resources and leave the old data PVC orphaned.
Run this runbook once, on a machine with `kubectl` + registry access, **before**
merging `rename-pio` → `master` (CI auto-deploys on master push).

Key facts that make this safe:
- The crypto AAD stays `webshare-proxy/v1/`, so a byte-copied `data.db` and
  `master.key` decrypt unchanged on the new pod. **Do not change the AAD.**
- The MetalLB IP `192.168.2.241` is requested by *both* the old `webshare-proxy`
  and the new `pio-proxy` Service. Two LoadBalancers cannot share one IP, so the
  old Service MUST be deleted before the new one can claim it (handled in step 4).
- Deployment is single-replica `Recreate` (SQLite single-writer), so expect a
  short downtime window during cutover — fine for a homelab.

All commands assume `-n default`.

---

## 0. Build & push the `pio` image

The registry currently has `xgfan/pia` (old). The new deployment pulls
`xgfan/pio`, which doesn't exist yet. Pushing the `rename-pio` *branch* does NOT
trigger Woodpecker (`when: branch: master`), so build & push manually first:

```sh
docker buildx build --platform linux/amd64 \
  -t docker.test4x.com/xgfan/pio:latest --push .
# (or run the .woodpecker.yaml "build" step once against the branch)
```

## 1. Stop the old writer to release the source PVC (RWO)

```sh
kubectl scale deployment/webshare --replicas=0
kubectl wait --for=delete pod -l app=webshare --timeout=120s
```

## 2. Create the destination PVC

```sh
kubectl apply -f - <<'YAML'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: pio-data
  namespace: default
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
YAML
```

## 3. Copy `master.key` + `data.db` (and everything) old → new

A single pod mounts both PVCs and copies with `-a` to preserve the `0600`
`master.key` mode. Both RWO volumes attach to this one pod's node — OK.

```sh
kubectl apply -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: pvc-migrate
  namespace: default
spec:
  restartPolicy: Never
  securityContext:
    fsGroup: 1000
  containers:
    - name: copy
      image: alpine:3.21
      command: ["sh", "-c", "cp -av /src/. /dst/ && ls -la /dst && echo COPY_DONE"]
      volumeMounts:
        - { name: src, mountPath: /src }
        - { name: dst, mountPath: /dst }
  volumes:
    - name: src
      persistentVolumeClaim: { claimName: webshare-data }
    - name: dst
      persistentVolumeClaim: { claimName: pio-data }
YAML

kubectl wait --for=condition=Ready pod/pvc-migrate --timeout=120s || true
kubectl logs -f pod/pvc-migrate          # expect master.key, data.db, COPY_DONE
kubectl delete pod/pvc-migrate
```

## 4. Cut over — free the old objects, apply the new manifest

Deleting `svc/webshare-proxy` first releases MetalLB IP `192.168.2.241` so the
new `pio-proxy` can claim it.

```sh
kubectl delete deployment/webshare svc/webshare svc/webshare-proxy ingress/webshare

# From a checkout of the rename-pio branch:
kubectl apply -f deploy/k8s.yaml
kubectl set image deployment/pio pio=docker.test4x.com/xgfan/pio:latest
kubectl rollout status deployment/pio --timeout=180s
```

## 5. Verify the migrated data on the new pod (loopback API)

```sh
POD=$(kubectl get pod -l app=pio -o jsonpath='{.items[0].metadata.name}')
kubectl exec "$POD" -- wget -qO- http://127.0.0.1:9090/api/v1/keys
kubectl exec "$POD" -- wget -qO- http://127.0.0.1:9090/api/v1/upstreams
kubectl exec "$POD" -- wget -qO- http://127.0.0.1:9090/api/v1/users
```

Expect your existing API keys / upstreams / users to be present and decrypted
(decryption proves `master.key` + AAD carried over). Confirm the LoadBalancer:

```sh
kubectl get svc/pio-proxy   # EXTERNAL-IP should be 192.168.2.241
```

## 6. Merge and clean up

Once verified, merge `rename-pio` → `master`. CI re-applies the (already-applied)
manifest in place and rolls the freshly-built `:<sha>` image onto `deployment/pio`.

After a few days of confidence, reclaim the old volume:

```sh
kubectl delete pvc/webshare-data
```

## DNS / TLS

`pio.test4x.com` is served by the existing Traefik ingress and covered by the
`wildcard-test4x-com-tls` (`*.test4x.com`) cert — **no new certificate needed**.
If DNS for `test4x.com` is a wildcard `*.test4x.com` → Traefik record, nothing to
do. If records are per-host, add an A/CNAME for `pio.test4x.com` pointing at the
same target as the old `pia.test4x.com`.
