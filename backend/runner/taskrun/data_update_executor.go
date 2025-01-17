package taskrun

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/pkg/errors"

	storepb "github.com/bytebase/bytebase/proto/generated-go/store"

	"github.com/bytebase/bytebase/backend/common"
	"github.com/bytebase/bytebase/backend/common/log"
	"github.com/bytebase/bytebase/backend/component/config"
	"github.com/bytebase/bytebase/backend/component/dbfactory"
	"github.com/bytebase/bytebase/backend/component/state"
	enterprise "github.com/bytebase/bytebase/backend/enterprise/api"
	"github.com/bytebase/bytebase/backend/runner/schemasync"

	api "github.com/bytebase/bytebase/backend/legacyapi"
	"github.com/bytebase/bytebase/backend/plugin/db"
	"github.com/bytebase/bytebase/backend/plugin/db/oracle"
	"github.com/bytebase/bytebase/backend/plugin/parser/base"
	"github.com/bytebase/bytebase/backend/store"
	"github.com/bytebase/bytebase/backend/store/model"
)

// NewDataUpdateExecutor creates a data update (DML) task executor.
func NewDataUpdateExecutor(store *store.Store, dbFactory *dbfactory.DBFactory, license enterprise.LicenseService, stateCfg *state.State, schemaSyncer *schemasync.Syncer, profile *config.Profile) Executor {
	return &DataUpdateExecutor{
		store:        store,
		dbFactory:    dbFactory,
		license:      license,
		stateCfg:     stateCfg,
		schemaSyncer: schemaSyncer,
		profile:      profile,
	}
}

// DataUpdateExecutor is the data update (DML) task executor.
type DataUpdateExecutor struct {
	store        *store.Store
	dbFactory    *dbfactory.DBFactory
	license      enterprise.LicenseService
	stateCfg     *state.State
	schemaSyncer *schemasync.Syncer
	profile      *config.Profile
}

// RunOnce will run the data update (DML) task executor once.
func (exec *DataUpdateExecutor) RunOnce(ctx context.Context, driverCtx context.Context, task *store.TaskMessage, taskRunUID int) (bool, *storepb.TaskRunResult, error) {
	payload := &storepb.TaskDatabaseUpdatePayload{}
	if err := common.ProtojsonUnmarshaler.Unmarshal([]byte(task.Payload), payload); err != nil {
		return true, nil, errors.Wrap(err, "invalid database data update payload")
	}

	sheetID := int(payload.SheetId)
	statement, err := exec.store.GetSheetStatementByID(ctx, sheetID)
	if err != nil {
		return true, nil, err
	}
	priorBackupDetail, err := exec.backupData(ctx, driverCtx, statement, payload, task)
	if err != nil {
		return true, nil, err
	}
	version := model.Version{Version: payload.SchemaVersion}
	terminated, result, err := runMigration(ctx, driverCtx, exec.store, exec.dbFactory, exec.stateCfg, exec.profile, task, taskRunUID, db.Data, statement, version, &sheetID)
	if result != nil {
		// Save prior backup detail to task run result.
		result.PriorBackupDetail = priorBackupDetail
	}
	return terminated, result, err
}

func (exec *DataUpdateExecutor) backupData(
	ctx context.Context,
	driverCtx context.Context,
	statement string,
	payload *storepb.TaskDatabaseUpdatePayload,
	task *store.TaskMessage,
) (*storepb.PriorBackupDetail, error) {
	if payload.PreUpdateBackupDetail == nil || payload.PreUpdateBackupDetail.Database == "" {
		return nil, nil
	}

	instance, err := exec.store.GetInstanceV2(ctx, &store.FindInstanceMessage{UID: &task.InstanceID})
	if err != nil {
		return nil, errors.Wrap(err, "failed to get instance")
	}
	database, err := exec.store.GetDatabaseV2(ctx, &store.FindDatabaseMessage{UID: task.DatabaseID})
	if err != nil {
		return nil, errors.Wrap(err, "failed to get database")
	}
	issue, err := exec.store.GetIssueV2(ctx, &store.FindIssueMessage{PipelineID: &task.PipelineID})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to find issue for pipeline %v", task.PipelineID)
	}
	if issue == nil {
		return nil, errors.Errorf("issue not found for pipeline %v", task.PipelineID)
	}

	sourceDatabaseName := common.FormatDatabase(database.InstanceID, database.DatabaseName)
	// Format: instances/{instance}/databases/{database}
	targetDatabaseName := payload.PreUpdateBackupDetail.Database
	var backupDatabase *store.DatabaseMessage
	var backupDriver db.Driver

	backupInstanceID, backupDatabaseName, err := common.GetInstanceDatabaseID(targetDatabaseName)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse backup database")
	}

	if instance.Engine != storepb.Engine_POSTGRES {
		backupDatabase, err = exec.store.GetDatabaseV2(ctx, &store.FindDatabaseMessage{InstanceID: &backupInstanceID, DatabaseName: &backupDatabaseName})
		if err != nil {
			return nil, errors.Wrap(err, "failed to get backup database")
		}
		if backupDatabase == nil {
			return nil, errors.Errorf("backup database %q not found", targetDatabaseName)
		}
		backupDriver, err = exec.dbFactory.GetAdminDatabaseDriver(driverCtx, instance, backupDatabase, db.ConnectionContext{})
		if err != nil {
			return nil, errors.Wrap(err, "failed to get backup database driver")
		}
		defer backupDriver.Close(driverCtx)
	}

	driver, err := exec.dbFactory.GetAdminDatabaseDriver(driverCtx, instance, database, db.ConnectionContext{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to get database driver")
	}
	defer driver.Close(driverCtx)

	tc := base.TransformContext{
		InstanceID:              instance.ResourceID,
		GetDatabaseMetadataFunc: BuildGetDatabaseMetadataFunc(exec.store),
	}
	if instance.Engine == storepb.Engine_ORACLE {
		oracleDriver, ok := driver.(*oracle.Driver)
		if ok {
			if version, err := oracleDriver.GetVersion(); err == nil {
				tc.Version = version
			}
		}
	}

	prefix := "_" + time.Now().Format("20060102150405")
	statements, err := base.TransformDMLToSelect(ctx, instance.Engine, tc, statement, database.DatabaseName, backupDatabaseName, prefix)
	if err != nil {
		return nil, errors.Wrap(err, "failed to transform DML to select")
	}

	priorBackupDetail := &storepb.PriorBackupDetail{}
	for _, statement := range statements {
		if _, err := driver.Execute(driverCtx, statement.Statement, db.ExecuteOptions{}); err != nil {
			return nil, errors.Wrapf(err, "failed to execute backup statement %q", statement.Statement)
		}
		switch instance.Engine {
		case storepb.Engine_TIDB:
			if _, err := driver.Execute(driverCtx, fmt.Sprintf("ALTER TABLE `%s`.`%s` COMMENT = 'issue %d'", backupDatabaseName, statement.TargetTableName, issue.UID), db.ExecuteOptions{}); err != nil {
				return nil, errors.Wrap(err, "failed to set table comment")
			}
		case storepb.Engine_MYSQL:
			if _, err := driver.Execute(driverCtx, fmt.Sprintf("ALTER TABLE `%s`.`%s` COMMENT = 'issue %d'", backupDatabaseName, statement.TargetTableName, issue.UID), db.ExecuteOptions{}); err != nil {
				return nil, errors.Wrap(err, "failed to set table comment")
			}
		case storepb.Engine_MSSQL:
			if _, err := backupDriver.Execute(driverCtx, fmt.Sprintf("EXEC sp_addextendedproperty 'MS_Description', 'issue %d', 'SCHEMA', 'dbo', 'TABLE', '%s'", issue.UID, statement.TargetTableName), db.ExecuteOptions{}); err != nil {
				return nil, errors.Wrap(err, "failed to set table comment")
			}
		case storepb.Engine_POSTGRES:
			if _, err := driver.Execute(driverCtx, fmt.Sprintf(`COMMENT ON TABLE "%s"."%s" IS 'issue %d'`, backupDatabaseName, statement.TargetTableName, issue.UID), db.ExecuteOptions{}); err != nil {
				return nil, errors.Wrap(err, "failed to set table comment")
			}
		case storepb.Engine_ORACLE:
			if _, err := driver.Execute(driverCtx, fmt.Sprintf(`COMMENT ON TABLE "%s"."%s" IS 'issue %d'`, backupDatabaseName, statement.TargetTableName, issue.UID), db.ExecuteOptions{}); err != nil {
				return nil, errors.Wrap(err, "failed to set table comment")
			}
		}

		priorBackupDetail.Items = append(priorBackupDetail.Items, &storepb.PriorBackupDetail_Item{
			SourceTable: &storepb.PriorBackupDetail_Item_Table{
				Database: sourceDatabaseName,
				Table:    statement.SourceTableName,
			},
			TargetTable: &storepb.PriorBackupDetail_Item_Table{
				Database: targetDatabaseName,
				Schema:   "",
				Table:    statement.TargetTableName,
			},
			StartPosition: statement.StartPosition,
			EndPosition:   statement.EndPosition,
		})

		if _, err := exec.store.CreateIssueComment(ctx, &store.IssueCommentMessage{
			IssueUID: issue.UID,
			Payload: &storepb.IssueCommentPayload{
				Event: &storepb.IssueCommentPayload_TaskPriorBackup_{
					TaskPriorBackup: &storepb.IssueCommentPayload_TaskPriorBackup{
						Task:     common.FormatTask(issue.Project.ResourceID, task.PipelineID, task.StageID, task.ID),
						Database: backupDatabaseName,
						Tables: []*storepb.IssueCommentPayload_TaskPriorBackup_Table{
							{
								Schema: "",
								Table:  statement.TargetTableName,
							},
						},
					},
				},
			},
		}, api.SystemBotID); err != nil {
			slog.Warn("failed to create issue comment", "task", task.ID, log.BBError(err))
		}
	}

	if instance.Engine != storepb.Engine_POSTGRES {
		if err := exec.schemaSyncer.SyncDatabaseSchema(ctx, backupDatabase, false /* force */); err != nil {
			slog.Error("failed to sync backup database schema",
				slog.String("database", payload.PreUpdateBackupDetail.Database),
				log.BBError(err),
			)
		}
	} else {
		if err := exec.schemaSyncer.SyncDatabaseSchema(ctx, database, false /* force */); err != nil {
			slog.Error("failed to sync backup database schema",
				slog.String("database", fmt.Sprintf("/instances/%s/databases/%s", instance.ResourceID, database.DatabaseName)),
				log.BBError(err),
			)
		}
	}

	return priorBackupDetail, nil
}

func BuildGetDatabaseMetadataFunc(storeInstance *store.Store) base.GetDatabaseMetadataFunc {
	return func(ctx context.Context, instanceID, databaseName string) (string, *model.DatabaseMetadata, error) {
		database, err := storeInstance.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
			InstanceID:   &instanceID,
			DatabaseName: &databaseName,
		})
		if err != nil {
			return "", nil, err
		}
		if database == nil {
			return "", nil, nil
		}
		databaseMetadata, err := storeInstance.GetDBSchema(ctx, database.UID)
		if err != nil {
			return "", nil, err
		}
		if databaseMetadata == nil {
			return "", nil, nil
		}
		return databaseName, databaseMetadata.GetDatabaseMetadata(), nil
	}
}
