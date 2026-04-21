# S3 fencing for RamenDR

## Problem

RamenDR backs up VolumeReplicationGroup (VRG) manifests and related cluster state to a shared object store (S3). During some failover transitions, **both** the former primary and the DR cluster can be reachable from the hub. Without coordination, both can write to the same bucket or prefix, which can:

- Interleave or overwrite backups from two clusters
- Destroy a single well-defined “last good backup”
- Risk inconsistent restores

S3 does not provide split-brain resolution for this use case. **Correctness depends on Ramen’s control plane** deciding which cluster may write.

## Objectives

| Objective | Approach |
|-----------|----------|
| Single writer | Only the cluster the hub treats as **placement home** for the workload may write VRG and kube-object backups to S3 when VRG is `primary`. |
| Safe restore | Restore continues to use **completed** kube-object captures (`status.kubeObjectProtection.captureToRecoverFrom`) and reads VRG state from S3 only when hub-directed. |
| Deterministic backup selection | Kube-object recovery point remains the `KubeObjectsCaptureIdentifier` in status; VRG manifest mirror uses a stable key per VRG with completion recorded in `status.lastVRGObjectBackupTime`. |
| Role transition | After failover/relocate, placement and VRG replication state move; the hub sets `s3-backup-write-allowed` on the new primary’s VRG and clears it on the old primary. |
| Robustness | If placement is unknown, writes stay disabled until the hub can authorize a writer. If S3 is unreachable, existing fail-fast behavior applies. |

## Architecture

### Hub authority

The hub (DRPlacementControl reconciler) encodes **who may upload** using an annotation on the VRG object delivered via ManifestWork:

`drplacementcontrol.ramendr.openshift.io/s3-backup-write-allowed`: `true` or `false`

Semantics:

- **`true`** — This managed cluster is the **current placement home** *and* the VRG is **primary** on that cluster. The VRG controller may perform S3 uploads (PV/PVC metadata, kube-object captures, VRG object mirror).
- **`false`** — This cluster must **not** perform S3 uploads for this VRG, even if spec temporarily still says `primary` (stale manifest or transition window).

The hub computes the authorized cluster from, in order:

1. The Placement / PlacementRule **cluster decision** (live placement), when available  
2. `DRPlacementControl.status.preferredDecision.clusterName`  
3. `DRPlacementControl.spec.preferredCluster`  

The VRG on cluster `C` receives `true` only if `C` matches that authoritative name and `spec.replicationState` is `Primary`.

### Data plane (VRG controller)

On the managed cluster, before any S3 **write** (upload or delete that is part of a capture cycle):

1. `spec.replicationState` must be `primary` (existing behavior for primary reconcile path).  
2. `s3-backup-write-allowed` must be **`true`**. Any other value (including absent, for strict enforcement after hub rollout) blocks uploads until the hub updates ManifestWork.

Blocked writes surface as conditions with reason `S3BackupFenced` so operators can see that the cluster is waiting on hub placement rather than on storage.

### Relationship to kube object protection

Periodic kube-object captures and the VRG JSON mirror share the same fence. **Restore** paths remain **read** operations and are not blocked by the writer annotation; they follow existing failover/relocate logic and `captureToRecoverFrom`.

### Failure scenarios

- **Both clusters up** — Only one cluster has `s3-backup-write-allowed=true` at steady state.  
- **One cluster down** — The surviving cluster’s VRG is updated by the hub when placement changes; S3 writes follow the surviving primary only when authorized.  
- **S3 profile down** — Uploads fail without claiming success; no partial “completed” metadata for the VRG object path beyond what already exists.

## VRG status

`status.lastVRGObjectBackupTime` records when the VRG manifest was last **successfully** written to all configured S3 profiles, giving a deterministic “last completed VRG object backup” marker for automation and debugging.

## Migration

After upgrading the hub, DRPC reconciliation updates ManifestWork for all managed VRGs with the new annotation. Until that completes, VRGs without `true` do not upload (fail-safe). Clusters should run a compatible hub version before or together with the dr-cluster operator that enforces the gate.

## References

- `internal/controller/drplacementcontrol.go` — `updateVRGOptionalFields`, placement home resolution  
- `internal/controller/vrg_s3_fence.go` — VRG-side gate  
- `api/v1alpha1/volumereplicationgroup_types.go` — `lastVRGObjectBackupTime`  
