package orchestrator

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	v1 "github.com/garethgeorge/resticui/gen/go/v1"
	"github.com/garethgeorge/resticui/internal/oplog/indexutil"
	"github.com/garethgeorge/resticui/internal/protoutil"
	"github.com/garethgeorge/resticui/pkg/restic"
	"github.com/gitploy-io/cronexpr"
	"go.uber.org/zap"
)

// BackupTask is a scheduled backup operation.
type BackupTask struct {
	name         string
	orchestrator *Orchestrator // owning orchestrator
	plan         *v1.Plan
	op           *v1.Operation
	scheduler    func(curTime time.Time) *time.Time
	cancel       atomic.Pointer[context.CancelFunc] // nil unless operation is running.
}

var _ Task = &BackupTask{}

func NewScheduledBackupTask(orchestrator *Orchestrator, plan *v1.Plan) (*BackupTask, error) {
	sched, err := cronexpr.ParseInLocation(plan.Cron, time.Now().Location().String())
	if err != nil {
		return nil, fmt.Errorf("failed to parse schedule %q: %w", plan.Cron, err)
	}

	return &BackupTask{
		name:         fmt.Sprintf("backup for plan %q", plan.Id),
		orchestrator: orchestrator,
		plan:         plan,
		scheduler: func(curTime time.Time) *time.Time {
			next := sched.Next(curTime)
			return &next
		},
	}, nil
}

func NewOneofBackupTask(orchestrator *Orchestrator, plan *v1.Plan, at time.Time) *BackupTask {
	didOnce := false
	return &BackupTask{
		name:         fmt.Sprintf("onetime backup for plan %q", plan.Id),
		orchestrator: orchestrator,
		plan:         plan,
		scheduler: func(curTime time.Time) *time.Time {
			if didOnce {
				return nil
			}
			didOnce = true
			return &at
		},
	}
}

func (t *BackupTask) Name() string {
	return t.name
}

func (t *BackupTask) Next(now time.Time) *time.Time {
	next := t.scheduler(now)
	if next == nil {
		return nil
	}

	t.op = &v1.Operation{
		PlanId:          t.plan.Id,
		RepoId:          t.plan.Repo,
		UnixTimeStartMs: timeToUnixMillis(*next),
		Status:          v1.OperationStatus_STATUS_PENDING,
		Op:              &v1.Operation_OperationBackup{},
	}

	if err := t.orchestrator.OpLog.Add(t.op); err != nil {
		zap.S().Errorf("task %v failed to add operation to oplog: %v", t.Name(), err)
		return nil
	}

	return next
}

func (t *BackupTask) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	t.cancel.Store(&cancel)
	err := backupHelper(ctx, t.orchestrator, t.plan, t.op)
	t.op = nil
	t.cancel.Store(nil)
	return err
}

func (t *BackupTask) Cancel(status v1.OperationStatus) error {
	if t.op == nil {
		return nil
	}

	cancel := t.cancel.Load()
	if cancel != nil && status == v1.OperationStatus_STATUS_USER_CANCELLED {
		(*cancel)() // try to interrupt the running operation.
	}

	t.op.Status = status
	t.op.UnixTimeEndMs = curTimeMillis()
	return t.orchestrator.OpLog.Update(t.op)
}

// backupHelper does a backup.
func backupHelper(ctx context.Context, orchestrator *Orchestrator, plan *v1.Plan, op *v1.Operation) error {
	backupOp := &v1.Operation_OperationBackup{
		OperationBackup: &v1.OperationBackup{},
	}

	startTime := time.Now()
	op.Op = backupOp
	op.UnixTimeStartMs = curTimeMillis()

	err := WithOperation(orchestrator.OpLog, op, func() error {
		zap.L().Info("Starting backup", zap.String("plan", plan.Id), zap.Int64("opId", op.Id))
		repo, err := orchestrator.GetRepo(plan.Repo)
		if err != nil {
			return fmt.Errorf("couldn't get repo %q: %w", plan.Repo, err)
		}

		lastSent := time.Now() // debounce progress updates, these can endup being very frequent.
		summary, err := repo.Backup(ctx, plan, func(entry *restic.BackupProgressEntry) {
			if time.Since(lastSent) < 250*time.Millisecond {
				return
			}
			lastSent = time.Now()

			backupOp.OperationBackup.LastStatus = protoutil.BackupProgressEntryToProto(entry)
			if err := orchestrator.OpLog.Update(op); err != nil {
				zap.S().Errorf("failed to update oplog with progress for backup: %v", err)
			}
		})
		if err != nil {
			return fmt.Errorf("repo.Backup for repo %q: %w", plan.Repo, err)
		}

		op.SnapshotId = summary.SnapshotId
		backupOp.OperationBackup.LastStatus = protoutil.BackupProgressEntryToProto(summary)
		if backupOp.OperationBackup.LastStatus == nil {
			return fmt.Errorf("expected a final backup progress entry, got nil")
		}

		zap.L().Info("Backup complete", zap.String("plan", plan.Id), zap.Duration("duration", time.Since(startTime)), zap.Any("summary", summary))
		return nil
	})
	if err != nil {
		return fmt.Errorf("backup operation: %w", err)
	}

	// this could alternatively be scheduled as a separate task, but it probably makes sense to index snapshots immediately after a backup.
	if err := indexSnapshotsHelper(ctx, orchestrator, plan); err != nil {
		return fmt.Errorf("reindexing snapshots after backup operation: %w", err)
	}

	if plan.Retention != nil {
		orchestrator.ScheduleTask(NewOneofForgetTask(orchestrator, plan, time.Now()))
	}

	return nil
}

func indexSnapshotsHelper(ctx context.Context, orchestrator *Orchestrator, plan *v1.Plan) error {
	repo, err := orchestrator.GetRepo(plan.Repo)
	if err != nil {
		return fmt.Errorf("couldn't get repo %q: %w", plan.Repo, err)
	}

	snapshots, err := repo.SnapshotsForPlan(ctx, plan)
	if err != nil {
		return fmt.Errorf("get snapshots for plan %q: %w", plan.Id, err)
	}

	startTime := time.Now()
	alreadyIndexed := 0
	var indexOps []*v1.Operation
	for _, snapshot := range snapshots {
		ops, err := orchestrator.OpLog.GetBySnapshotId(snapshot.Id, indexutil.CollectAll())
		if err != nil {
			return fmt.Errorf("HasIndexSnapshot for snapshot %q: %w", snapshot.Id, err)
		}

		if containsSnapshotOperation(ops) {
			alreadyIndexed += 1
			continue
		}

		snapshotProto := protoutil.SnapshotToProto(snapshot)
		indexOps = append(indexOps, &v1.Operation{
			RepoId:          plan.Repo,
			PlanId:          plan.Id,
			UnixTimeStartMs: snapshotProto.UnixTimeMs,
			UnixTimeEndMs:   snapshotProto.UnixTimeMs,
			Status:          v1.OperationStatus_STATUS_SUCCESS,
			SnapshotId:      snapshotProto.Id,
			Op: &v1.Operation_OperationIndexSnapshot{
				OperationIndexSnapshot: &v1.OperationIndexSnapshot{
					Snapshot: snapshotProto,
				},
			},
		})
	}

	if err := orchestrator.OpLog.BulkAdd(indexOps); err != nil {
		return fmt.Errorf("BulkAdd snapshot operations: %w", err)
	}

	zap.L().Debug("Indexed snapshots",
		zap.String("plan", plan.Id),
		zap.Duration("duration", time.Since(startTime)),
		zap.Int("alreadyIndexed", alreadyIndexed),
		zap.Int("newlyAdded", len(snapshots)-alreadyIndexed),
	)

	return err
}

func containsSnapshotOperation(ops []*v1.Operation) bool {
	for _, op := range ops {
		if _, ok := op.Op.(*v1.Operation_OperationIndexSnapshot); ok {
			return true
		}
	}
	return false
}