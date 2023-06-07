package connsnowflake

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/PeerDB-io/peer-flow/generated/protos"
	"github.com/PeerDB-io/peer-flow/model"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/snowflakedb/gosnowflake"
)

//nolint:stylecheck
const (
	// all PeerDB specific tables should go in the internal schema.
	peerDBInternalSchema      = "_PEERDB_INTERNAL"
	mirrorJobsTableIdentifier = "PEERDB_MIRROR_JOBS"
	createMirrorJobsTableSQL  = `CREATE TABLE IF NOT EXISTS %s.%s(MIRROR_JOB_NAME STRING NOT NULL,OFFSET INT NOT NULL,
		SYNC_BATCH_ID INT NOT NULL,NORMALIZE_BATCH_ID INT NOT NULL)`
	rawTablePrefix                = "_PEERDB_RAW"
	createPeerDBInternalSchemaSQL = "CREATE TRANSIENT SCHEMA IF NOT EXISTS %s"
	createRawTableSQL             = `CREATE TABLE IF NOT EXISTS %s.%s(_PEERDB_UID STRING NOT NULL,
		_PEERDB_TIMESTAMP INT NOT NULL,_PEERDB_DESTINATION_TABLE_NAME STRING NOT NULL,_PEERDB_DATA STRING NOT NULL,
		_PEERDB_RECORD_TYPE INTEGER NOT NULL, _PEERDB_MATCH_DATA STRING,_PEERDB_BATCH_ID INT)`
	rawTableMultiValueInsertSQL = "INSERT INTO %s.%s VALUES%s"
	createNormalizedTableSQL    = "CREATE TABLE IF NOT EXISTS %s(%s)"
	toVariantColumnName         = "VAR_COLS"

	mergeStatementSQL = `MERGE INTO %s TARGET USING (WITH VARIANT_CONVERTED AS (SELECT _PEERDB_UID,_PEERDB_TIMESTAMP,
		TO_VARIANT(PARSE_JSON(_PEERDB_DATA)) %s,_PEERDB_RECORD_TYPE,_PEERDB_MATCH_DATA,_PEERDB_BATCH_ID FROM
		 _PEERDB_INTERNAL.%s WHERE _PEERDB_BATCH_ID > %d AND _PEERDB_BATCH_ID <= %d AND
		 _PEERDB_DESTINATION_TABLE_NAME = ?), FLATTENED AS
		 (SELECT _PEERDB_UID,_PEERDB_TIMESTAMP,_PEERDB_RECORD_TYPE,_PEERDB_MATCH_DATA,_PEERDB_BATCH_ID,%s
		 FROM VARIANT_CONVERTED), DEDUPLICATED_FLATTENED AS (SELECT RANKED.* FROM
		 (SELECT RANK() OVER (PARTITION BY %s ORDER BY _PEERDB_TIMESTAMP DESC) AS RANK,* FROM FLATTENED)
		 RANKED WHERE RANK=1)
		 SELECT * FROM DEDUPLICATED_FLATTENED) SOURCE ON TARGET.ID=SOURCE.ID
		 WHEN NOT MATCHED AND (SOURCE._PEERDB_RECORD_TYPE != 2) THEN INSERT (%s) VALUES(%s)
		 WHEN MATCHED AND (SOURCE._PEERDB_RECORD_TYPE != 2) THEN UPDATE SET %s
		 WHEN MATCHED AND (SOURCE._PEERDB_RECORD_TYPE = 2) THEN DELETE`
	getDistinctDestinationTableNames = `SELECT DISTINCT _PEERDB_DESTINATION_TABLE_NAME FROM %s.%s WHERE
	 _PEERDB_BATCH_ID > %d AND _PEERDB_BATCH_ID <= %d`
	insertJobMetadataSQL = "INSERT INTO %s.%s VALUES (?,?,?,?)"

	updateMetadataForSyncRecordsSQL      = "UPDATE %s.%s SET OFFSET=?, SYNC_BATCH_ID=? WHERE MIRROR_JOB_NAME=?"
	updateMetadataForNormalizeRecordsSQL = "UPDATE %s.%s SET NORMALIZE_BATCH_ID=? WHERE MIRROR_JOB_NAME=?"

	checkIfTableExistsSQL = `SELECT TO_BOOLEAN(COUNT(1)) FROM INFORMATION_SCHEMA.TABLES
	 WHERE TABLE_SCHEMA=? and TABLE_NAME=?`
	checkIfJobMetadataExistsSQL = "SELECT TO_BOOLEAN(COUNT(1)) FROM %s.%s WHERE MIRROR_JOB_NAME=?"
	getLastOffsetSQL            = "SELECT OFFSET FROM %s.%s WHERE MIRROR_JOB_NAME=?"
	getLastSyncBatchID_SQL      = "SELECT SYNC_BATCH_ID FROM %s.%s WHERE MIRROR_JOB_NAME=?"
	getLastNormalizeBatchID_SQL = "SELECT NORMALIZE_BATCH_ID FROM %s.%s WHERE MIRROR_JOB_NAME=?"
	dropTableIfExistsSQL        = "DROP TABLE IF EXISTS %s.%s"
	deleteJobMetadataSQL        = "DELETE FROM %s.%s WHERE MIRROR_JOB_NAME=?"

	syncRecordsChunkSize = 1024
)

type tableNameComponents struct {
	schemaIdentifier string
	tableIdentifier  string
}
type SnowflakeConnector struct {
	ctx                context.Context
	database           *sql.DB
	tableSchemaMapping map[string]*protos.TableSchema
}

type snowflakeRawRecord struct {
	uid                  string
	timestamp            int64
	destinationTableName string
	data                 string
	recordType           int
	matchData            string
	batchID              int64
	items                map[string]interface{}
}

// reads the PKCS8 private key from the received config and converts it into something that gosnowflake wants.
func readPKCS8PrivateKey(rawKey []byte) (*rsa.PrivateKey, error) {
	// pem.Decode has weird return values, no err as such
	PEMBlock, _ := pem.Decode(rawKey)
	if PEMBlock == nil {
		return nil, fmt.Errorf("failed to decode private key PEM block")
	}
	privateKeyAny, err := x509.ParsePKCS8PrivateKey(PEMBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key PEM block as PKCS8: %w", err)
	}
	privateKeyRSA, ok := privateKeyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key does not appear to RSA as expected")
	}

	return privateKeyRSA, nil
}

func NewSnowflakeConnector(ctx context.Context,
	snowflakeProtoConfig *protos.SnowflakeConfig) (*SnowflakeConnector, error) {
	PrivateKeyRSA, err := readPKCS8PrivateKey([]byte(snowflakeProtoConfig.PrivateKey))
	if err != nil {
		return nil, err
	}

	snowflakeConfig := gosnowflake.Config{
		Account:          snowflakeProtoConfig.AccountId,
		User:             snowflakeProtoConfig.Username,
		Authenticator:    gosnowflake.AuthTypeJwt,
		PrivateKey:       PrivateKeyRSA,
		Database:         snowflakeProtoConfig.Database,
		Warehouse:        snowflakeProtoConfig.Warehouse,
		Role:             snowflakeProtoConfig.Role,
		RequestTimeout:   time.Duration(snowflakeProtoConfig.QueryTimeout),
		DisableTelemetry: true,
	}
	snowflakeConfigDSN, err := gosnowflake.DSN(&snowflakeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to get DSN from Snowflake config: %w", err)
	}

	database, err := sql.Open("snowflake", snowflakeConfigDSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open connection to Snowflake peer: %w", err)
	}
	// checking if connection was actually established, since sql.Open doesn't guarantee that
	err = database.PingContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open connection to Snowflake peer: %w", err)
	}

	return &SnowflakeConnector{
		ctx:                ctx,
		database:           database,
		tableSchemaMapping: nil,
	}, nil
}

func (c *SnowflakeConnector) Close() error {
	if c == nil || c.database == nil {
		return nil
	}

	err := c.database.Close()
	if err != nil {
		return fmt.Errorf("error while closing connection to Snowflake peer: %w", err)
	}
	return nil
}

func (c *SnowflakeConnector) ConnectionActive() bool {
	if c == nil || c.database == nil {
		return false
	}
	return c.database.PingContext(c.ctx) == nil
}

func (c *SnowflakeConnector) NeedsSetupMetadataTables() bool {
	result, err := c.checkIfTableExists(peerDBInternalSchema, mirrorJobsTableIdentifier)
	if err != nil {
		return true
	}
	return !result
}

func (c *SnowflakeConnector) SetupMetadataTables() error {
	createMetadataTablesTx, err := c.database.BeginTx(c.ctx, nil)
	if err != nil {
		return fmt.Errorf("unable to begin transaction for creating metadata tables: %w", err)
	}
	err = c.createPeerDBInternalSchema(createMetadataTablesTx)
	if err != nil {
		return err
	}
	_, err = createMetadataTablesTx.ExecContext(c.ctx, fmt.Sprintf(createMirrorJobsTableSQL,
		peerDBInternalSchema, mirrorJobsTableIdentifier))
	if err != nil {
		return fmt.Errorf("error while setting up mirror jobs table: %w", err)
	}
	err = createMetadataTablesTx.Commit()
	if err != nil {
		return fmt.Errorf("unable to commit transaction for creating metadata tables: %w", err)
	}
	return nil
}

func (c *SnowflakeConnector) GetLastOffset(jobName string) (*protos.LastSyncState, error) {
	rows, err := c.database.QueryContext(c.ctx, fmt.Sprintf(getLastOffsetSQL,
		peerDBInternalSchema, mirrorJobsTableIdentifier), jobName)
	if err != nil {
		return nil, fmt.Errorf("error querying Snowflake peer for last syncedID: %w", err)
	}

	var result int64
	if !rows.Next() {
		log.Warnf("No row found for job %s, returning nil", jobName)
		return nil, nil
	}
	err = rows.Scan(&result)
	if err != nil {
		return nil, fmt.Errorf("error while reading result row: %w", err)
	}
	if result == 0 {
		log.Warnf("Assuming zero offset means no sync has happened for job %s, returning nil", jobName)
		return nil, nil
	}

	return &protos.LastSyncState{
		Checkpoint: result,
	}, nil
}

func (c *SnowflakeConnector) GetLastSyncBatchID(jobName string) (int64, error) {
	rows, err := c.database.QueryContext(c.ctx, fmt.Sprintf(getLastSyncBatchID_SQL, peerDBInternalSchema,
		mirrorJobsTableIdentifier), jobName)
	if err != nil {
		return 0, fmt.Errorf("error querying Snowflake peer for last syncBatchId: %w", err)
	}

	var result int64
	if !rows.Next() {
		log.Warnf("No row found for job %s, returning 0", jobName)
		return 0, nil
	}
	err = rows.Scan(&result)
	if err != nil {
		return 0, fmt.Errorf("error while reading result row: %w", err)
	}
	return result, nil
}

func (c *SnowflakeConnector) GetLastNormalizeBatchID(jobName string) (int64, error) {
	rows, err := c.database.QueryContext(c.ctx, fmt.Sprintf(getLastNormalizeBatchID_SQL, peerDBInternalSchema,
		mirrorJobsTableIdentifier), jobName)
	if err != nil {
		return 0, fmt.Errorf("error querying Snowflake peer for last normalizeBatchId: %w", err)
	}

	var result int64
	if !rows.Next() {
		log.Warnf("No row found for job %s, returning 0", jobName)
		return 0, nil
	}
	err = rows.Scan(&result)
	if err != nil {
		return 0, fmt.Errorf("error while reading result row: %w", err)
	}
	return result, nil
}

func (c *SnowflakeConnector) getDistinctTableNamesInBatch(flowJobName string, syncBatchID int64,
	normalizeBatchID int64) ([]string, error) {
	rawTableIdentifier := getRawTableIdentifier(flowJobName)

	rows, err := c.database.QueryContext(c.ctx, fmt.Sprintf(getDistinctDestinationTableNames, peerDBInternalSchema,
		rawTableIdentifier, normalizeBatchID, syncBatchID))
	if err != nil {
		return nil, fmt.Errorf("error while retrieving table names for normalization: %w", err)
	}

	var result string
	destinationTableNames := make([]string, 0)
	for rows.Next() {
		err = rows.Scan(&result)
		if err != nil {
			return nil, fmt.Errorf("failed to read row: %w", err)
		}
		destinationTableNames = append(destinationTableNames, result)
	}
	return destinationTableNames, nil
}

func (c *SnowflakeConnector) GetTableSchema(req *protos.GetTableSchemaInput) (*protos.TableSchema, error) {
	log.Errorf("panicking at call to GetTableSchema for Snowflake flow connector")
	panic("GetTableSchema is not implemented for the Snowflake flow connector")
}

func (c *SnowflakeConnector) SetupNormalizedTable(
	req *protos.SetupNormalizedTableInput) (*protos.SetupNormalizedTableOutput, error) {
	normalizedTableNameComponents, err := parseTableName(req.TableIdentifier)
	if err != nil {
		return nil, fmt.Errorf("error while parsing table schema and name: %w", err)
	}
	tableAlreadyExists, err := c.checkIfTableExists(normalizedTableNameComponents.schemaIdentifier,
		normalizedTableNameComponents.tableIdentifier)
	if err != nil {
		return nil, fmt.Errorf("error occured while checking if normalized table exists: %w", err)
	}
	if tableAlreadyExists {
		return &protos.SetupNormalizedTableOutput{
			TableIdentifier: req.TableIdentifier,
			AlreadyExists:   true,
		}, nil
	}

	// convert the column names and types to Snowflake types
	normalizedTableCreateSQL := generateCreateTableSQLForNormalizedTable(req.TableIdentifier, req.SourceTableSchema)
	_, err = c.database.ExecContext(c.ctx, normalizedTableCreateSQL)
	if err != nil {
		return nil, fmt.Errorf("error while creating normalized table: %w", err)
	}

	return &protos.SetupNormalizedTableOutput{
		TableIdentifier: req.TableIdentifier,
		AlreadyExists:   false,
	}, nil
}

func (c *SnowflakeConnector) InitializeTableSchema(req map[string]*protos.TableSchema) error {
	c.tableSchemaMapping = req
	return nil
}

func (c *SnowflakeConnector) PullRecords(req *model.PullRecordsRequest) (*model.RecordBatch, error) {
	log.Errorf("panicking at call to PullRecords for Snowflake flow connector")
	panic("PullRecords is not implemented for the Snowflake flow connector")
}

func (c *SnowflakeConnector) SyncRecords(req *model.SyncRecordsRequest) (*model.SyncResponse, error) {
	rawTableIdentifier := getRawTableIdentifier(req.FlowJobName)
	log.Printf("pushing %d records to Snowflake table %s", len(req.Records.Records), rawTableIdentifier)

	syncBatchID, err := c.GetLastSyncBatchID(req.FlowJobName)
	if err != nil {
		return nil, fmt.Errorf("failed to get previous syncBatchID: %w", err)
	}
	syncBatchID = syncBatchID + 1
	records := make([]snowflakeRawRecord, 0)

	first := true
	var firstCP int64 = 0
	lastCP := req.Records.LastCheckPointID

	for _, record := range req.Records.Records {
		switch typedRecord := record.(type) {
		case *model.InsertRecord:
			itemsJSON, err := json.Marshal(typedRecord.Items)
			if err != nil {
				return nil, fmt.Errorf("failed to serialize insert record items to JSON: %w", err)
			}

			// add insert record to the raw table
			records = append(records, snowflakeRawRecord{
				uid:                  uuid.New().String(),
				timestamp:            time.Now().UnixNano(),
				destinationTableName: typedRecord.DestinationTableName,
				data:                 string(itemsJSON),
				recordType:           0,
				matchData:            "",
				batchID:              syncBatchID,
				items:                typedRecord.Items,
			})
		case *model.UpdateRecord:
			newItemsJSON, err := json.Marshal(typedRecord.NewItems)
			if err != nil {
				return nil, fmt.Errorf("failed to serialize update record new items to JSON: %w", err)
			}
			oldItemsJSON, err := json.Marshal(typedRecord.OldItems)
			if err != nil {
				return nil, fmt.Errorf("failed to serialize update record old items to JSON: %w", err)
			}

			// add update record to the raw table
			records = append(records, snowflakeRawRecord{
				uid:                  uuid.New().String(),
				timestamp:            time.Now().UnixNano(),
				destinationTableName: typedRecord.DestinationTableName,
				data:                 string(newItemsJSON),
				recordType:           1,
				matchData:            string(oldItemsJSON),
				batchID:              syncBatchID,
				items:                typedRecord.NewItems,
			})
		case *model.DeleteRecord:
			itemsJSON, err := json.Marshal(typedRecord.Items)
			if err != nil {
				return nil, fmt.Errorf("failed to serialize delete record items to JSON: %w", err)
			}

			// append delete record to the raw table
			records = append(records, snowflakeRawRecord{
				uid:                  uuid.New().String(),
				timestamp:            time.Now().UnixNano(),
				destinationTableName: typedRecord.DestinationTableName,
				data:                 string(itemsJSON),
				recordType:           2,
				matchData:            string(itemsJSON),
				batchID:              syncBatchID,
				items:                typedRecord.Items,
			})
		default:
			return nil, fmt.Errorf("record type %T not supported in Snowflake flow connector", typedRecord)
		}

		if first {
			firstCP = record.GetCheckPointID()
			first = false
		}
	}

	if len(records) == 0 {
		return &model.SyncResponse{
			FirstSyncedCheckPointID: 0,
			LastSyncedCheckPointID:  0,
			NumRecordsSynced:        0,
		}, nil
	}

	// transaction for SyncRecords
	syncRecordsTx, err := c.database.BeginTx(c.ctx, nil)
	if err != nil {
		return nil, err
	}
	// in case we return after error, ensure transaction is rolled back
	defer func() {
		deferErr := syncRecordsTx.Rollback()
		if deferErr != sql.ErrTxDone && deferErr != nil {
			log.Errorf("unexpected error while rolling back transaction for SyncRecords: %v", deferErr)
		}
	}()

	// inserting records into raw table.
	numRecords := len(records)
	for begin := 0; begin < numRecords; begin += syncRecordsChunkSize {
		end := begin + syncRecordsChunkSize

		if end > numRecords {
			end = numRecords
		}
		err = c.insertRecordsInRawTable(rawTableIdentifier, records[begin:end], syncRecordsTx)
		if err != nil {
			return nil, err
		}
	}

	// updating metadata with new offset and syncBatchID
	err = c.updateSyncMetadata(req.FlowJobName, lastCP, syncBatchID, syncRecordsTx)
	if err != nil {
		return nil, err
	}
	// transaction commits
	err = syncRecordsTx.Commit()
	if err != nil {
		return nil, err
	}

	return &model.SyncResponse{
		FirstSyncedCheckPointID: firstCP,
		LastSyncedCheckPointID:  lastCP,
		NumRecordsSynced:        int64(len(records)),
	}, nil
}

// NormalizeRecords normalizes raw table to destination table.
func (c *SnowflakeConnector) NormalizeRecords(req *model.NormalizeRecordsRequest) (*model.NormalizeResponse, error) {
	syncBatchID, err := c.GetLastSyncBatchID(req.FlowJobName)
	if err != nil {
		return nil, err
	}
	normalizeBatchID, err := c.GetLastNormalizeBatchID(req.FlowJobName)
	if err != nil {
		return nil, err
	}
	// normalize has caught up with sync, chill until more records are loaded.
	if syncBatchID == normalizeBatchID {
		return &model.NormalizeResponse{
			Done: true,
		}, nil
	}

	jobMetadataExists, err := c.jobMetadataExists(req.FlowJobName)
	if err != nil {
		return nil, err
	}
	// sync hasn't created job metadata yet, chill.
	if !jobMetadataExists {
		return &model.NormalizeResponse{
			Done: true,
		}, nil
	}
	destinationTableNames, err := c.getDistinctTableNamesInBatch(req.FlowJobName, syncBatchID, normalizeBatchID)
	if err != nil {
		return nil, err
	}

	// transaction for NormalizeRecords
	normalizeRecordsTx, err := c.database.BeginTx(c.ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("unable to begin transactions for NormalizeRecords: %w", err)
	}
	// in case we return after error, ensure transaction is rolled back
	defer func() {
		deferErr := normalizeRecordsTx.Rollback()
		if deferErr != sql.ErrTxDone && deferErr != nil {
			log.Errorf("unexpected error while rolling back transaction for NormalizeRecords: %v", deferErr)
		}
	}()
	// execute merge statements per table that uses CTEs to merge data into the normalized table
	for _, destinationTableName := range destinationTableNames {
		err = c.generateAndExecuteMergeStatement(destinationTableName,
			getRawTableIdentifier(req.FlowJobName),
			syncBatchID, normalizeBatchID, normalizeRecordsTx)
		if err != nil {
			return nil, err
		}
	}
	// updating metadata with new normalizeBatchID
	err = c.updateNormalizeMetadata(req.FlowJobName, syncBatchID, normalizeRecordsTx)
	if err != nil {
		return nil, err
	}
	// transaction commits
	err = normalizeRecordsTx.Commit()
	if err != nil {
		return nil, err
	}

	return &model.NormalizeResponse{
		Done:         true,
		StartBatchID: normalizeBatchID + 1,
		EndBatchID:   syncBatchID,
	}, nil
}

func (c *SnowflakeConnector) CreateRawTable(req *protos.CreateRawTableInput) (*protos.CreateRawTableOutput, error) {
	rawTableIdentifier := getRawTableIdentifier(req.FlowJobName)

	createRawTableTx, err := c.database.BeginTx(c.ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("unable to begin transaction for creation of raw table: %w", err)
	}
	err = c.createPeerDBInternalSchema(createRawTableTx)
	if err != nil {
		return nil, err
	}
	// there is no easy way to check if a table has the same schema in Snowflake, so just executing the CREATE TABLE IF NOT EXISTS blindly.
	_, err = createRawTableTx.ExecContext(c.ctx,
		fmt.Sprintf(createRawTableSQL, peerDBInternalSchema, rawTableIdentifier))
	if err != nil {
		return nil, fmt.Errorf("unable to create raw table: %w", err)
	}
	err = createRawTableTx.Commit()
	if err != nil {
		return nil, fmt.Errorf("unable to commit transaction for creation of raw table: %w", err)
	}

	return &protos.CreateRawTableOutput{
		TableIdentifier: rawTableIdentifier,
	}, nil
}

// EnsurePullability ensures that the table is pullable, implementing the Connector interface.
func (c *SnowflakeConnector) EnsurePullability(req *protos.EnsurePullabilityInput,
) (*protos.EnsurePullabilityOutput, error) {
	log.Errorf("panicking at call to EnsurePullability for Snowflake flow connector")
	panic("EnsurePullability is not implemented for the Snowflake flow connector")
}

// SetupReplication sets up replication for the source connector.
func (c *SnowflakeConnector) SetupReplication(req *protos.SetupReplicationInput) error {
	log.Errorf("panicking at call to SetupReplication for Snowflake flow connector")
	panic("SetupReplication is not implemented for the Snowflake flow connector")
}

func (c *SnowflakeConnector) PullFlowCleanup(jobName string) error {
	log.Errorf("panicking at call to PullFlowCleanup for Snowflake flow connector")
	panic("PullFlowCleanup is not implemented for the Snowflake flow connector")
}

func (c *SnowflakeConnector) SyncFlowCleanup(jobName string) error {
	syncFlowCleanupTx, err := c.database.BeginTx(c.ctx, nil)
	if err != nil {
		return fmt.Errorf("unable to begin transaction for sync flow cleanup: %w", err)
	}
	defer func() {
		deferErr := syncFlowCleanupTx.Rollback()
		if deferErr != sql.ErrTxDone && deferErr != nil {
			log.Errorf("unexpected error while rolling back transaction for flow cleanup: %v", deferErr)
		}
	}()

	_, err = syncFlowCleanupTx.ExecContext(c.ctx, fmt.Sprintf(dropTableIfExistsSQL, peerDBInternalSchema,
		getRawTableIdentifier(jobName)))
	if err != nil {
		return fmt.Errorf("unable to drop raw table: %w", err)
	}
	_, err = syncFlowCleanupTx.ExecContext(c.ctx,
		fmt.Sprintf(deleteJobMetadataSQL, peerDBInternalSchema, mirrorJobsTableIdentifier), jobName)
	if err != nil {
		return fmt.Errorf("unable to delete job metadata: %w", err)
	}
	err = syncFlowCleanupTx.Commit()
	if err != nil {
		return fmt.Errorf("unable to commit transaction for sync flow cleanup: %w", err)
	}
	return nil
}

func (c *SnowflakeConnector) checkIfTableExists(schemaIdentifier string, tableIdentifier string) (bool, error) {
	rows, err := c.database.QueryContext(c.ctx, checkIfTableExistsSQL, schemaIdentifier, tableIdentifier)
	if err != nil {
		return false, err
	}

	// this query is guaranteed to return exactly one row
	var result bool
	rows.Next()
	err = rows.Scan(&result)
	if err != nil {
		return false, fmt.Errorf("error while reading result row: %w", err)
	}
	return result, nil
}

func getSnowflakeTypeForGenericColumnType(colType string) string {
	switch colType {
	case model.ColumnTypeBoolean:
		return "BOOLEAN"
	case model.ColumnTypeInt32:
		return "INT"
	case model.ColumnTypeInt64:
		return "INT"
	case model.ColumnTypeFloat32:
		return "FLOAT"
	case model.ColumnTypeFloat64:
		return "FLOAT"
	case model.ColumnTypeString:
		return "STRING"
	case model.ColumnTypeTimestamp:
		// making an assumption that ColumnTypeTimestamp refers to a timestamp without timezone.
		return "TIMESTAMP_NTZ"
	default:
		return "STRING"
	}
}

func generateCreateTableSQLForNormalizedTable(sourceTableIdentifier string, sourceTableSchema *protos.TableSchema) string {
	createTableSQLArray := make([]string, 0, len(sourceTableSchema.Columns))
	for columnName, genericColumnType := range sourceTableSchema.Columns {
		if sourceTableSchema.PrimaryKeyColumn == strings.ToLower(columnName) {
			createTableSQLArray = append(createTableSQLArray, fmt.Sprintf("%s %s PRIMARY KEY,",
				strings.ToUpper(columnName), getSnowflakeTypeForGenericColumnType(genericColumnType)))
		} else {
			createTableSQLArray = append(createTableSQLArray, fmt.Sprintf("%s %s,", columnName,
				getSnowflakeTypeForGenericColumnType(genericColumnType)))
		}
	}
	return fmt.Sprintf(createNormalizedTableSQL, sourceTableIdentifier,
		strings.TrimSuffix(strings.Join(createTableSQLArray, ""), ","))
}

func generateMultiValueInsertSQL(tableIdentifier string, chunkSize int) string {
	// inferring the width of the raw table from the create table statement
	rawTableWidth := strings.Count(createRawTableSQL, ",") + 1

	return fmt.Sprintf(rawTableMultiValueInsertSQL, peerDBInternalSchema, tableIdentifier,
		strings.TrimSuffix(strings.Repeat(fmt.Sprintf("(%s),", strings.TrimSuffix(strings.Repeat("?,", rawTableWidth), ",")), chunkSize), ","))
}

func getRawTableIdentifier(jobName string) string {
	jobName = regexp.MustCompile("[^a-zA-Z0-9]+").ReplaceAllString(jobName, "_")
	return fmt.Sprintf("%s_%s", rawTablePrefix, jobName)
}

func (c *SnowflakeConnector) insertRecordsInRawTable(rawTableIdentifier string,
	snowflakeRawRecords []snowflakeRawRecord, syncRecordsTx *sql.Tx) error {
	rawRecordsData := make([]any, 0)

	for _, record := range snowflakeRawRecords {
		rawRecordsData = append(rawRecordsData, record.uid, record.timestamp, record.destinationTableName,
			record.data, record.recordType, record.matchData, record.batchID)
	}
	_, err := syncRecordsTx.ExecContext(c.ctx,
		generateMultiValueInsertSQL(rawTableIdentifier, len(snowflakeRawRecords)), rawRecordsData...)
	if err != nil {
		return fmt.Errorf("failed to insert record into raw table: %w", err)
	}
	return nil
}

func (c *SnowflakeConnector) generateAndExecuteMergeStatement(destinationTableIdentifier string,
	rawTableIdentifier string, syncBatchID int64, normalizeBatchID int64, normalizeRecordsTx *sql.Tx) error {
	normalizedTableSchema := c.tableSchemaMapping[destinationTableIdentifier]
	// TODO: switch this to function maps.Keys when it is moved into Go's stdlib
	columnNames := make([]string, 0, len(normalizedTableSchema.Columns))
	for columnName := range normalizedTableSchema.Columns {
		columnNames = append(columnNames, strings.ToUpper(columnName))
	}

	flattenedCastsSQLArray := make([]string, 0, len(normalizedTableSchema.Columns))
	for columnName, genericColumnType := range normalizedTableSchema.Columns {
		flattenedCastsSQLArray = append(flattenedCastsSQLArray, fmt.Sprintf("CAST(%s:%s AS %s) AS %s,", toVariantColumnName,
			columnName, getSnowflakeTypeForGenericColumnType(genericColumnType), columnName))
	}
	flattenedCastsSQL := strings.TrimSuffix(strings.Join(flattenedCastsSQLArray, ""), ",")

	insertColumnsSQL := strings.TrimSuffix(strings.Join(columnNames, ","), ",")
	insertValuesSQLArray := make([]string, 0, len(columnNames))
	for _, columnName := range columnNames {
		insertValuesSQLArray = append(insertValuesSQLArray, fmt.Sprintf("SOURCE.%s,", columnName))
	}
	insertValuesSQL := strings.TrimSuffix(strings.Join(insertValuesSQLArray, ""), ",")

	updateValuesSQLArray := make([]string, 0, len(columnNames))
	for _, columnName := range columnNames {
		updateValuesSQLArray = append(updateValuesSQLArray, fmt.Sprintf("%s=SOURCE.%s,", columnName, columnName))
	}
	updateValuesSQL := strings.TrimSuffix(strings.Join(updateValuesSQLArray, ""), ",")

	mergeStatement := fmt.Sprintf(mergeStatementSQL, destinationTableIdentifier, toVariantColumnName,
		rawTableIdentifier, normalizeBatchID, syncBatchID, flattenedCastsSQL,
		strings.ToUpper(normalizedTableSchema.PrimaryKeyColumn), insertColumnsSQL, insertValuesSQL, updateValuesSQL)

	_, err := normalizeRecordsTx.ExecContext(c.ctx, mergeStatement, destinationTableIdentifier)
	if err != nil {
		return fmt.Errorf("failed to merge records into %s: %w", destinationTableIdentifier, err)
	}

	return nil
}

// parseTableName parses a table name into schema and table name.
func parseTableName(tableName string) (*tableNameComponents, error) {
	parts := strings.Split(tableName, ".")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid table name: %s", tableName)
	}

	return &tableNameComponents{
		schemaIdentifier: parts[0],
		tableIdentifier:  parts[1],
	}, nil
}

func (c *SnowflakeConnector) jobMetadataExists(jobName string) (bool, error) {
	rows, err := c.database.QueryContext(c.ctx,
		fmt.Sprintf(checkIfJobMetadataExistsSQL, peerDBInternalSchema, mirrorJobsTableIdentifier), jobName)
	if err != nil {
		return false, fmt.Errorf("failed to check if job exists: %w", err)
	}

	var result bool
	rows.Next()
	err = rows.Scan(&result)
	if err != nil {
		return false, fmt.Errorf("error reading result row: %w", err)
	}
	return result, nil
}

func (c *SnowflakeConnector) updateSyncMetadata(flowJobName string, lastCP int64, syncBatchID int64, syncRecordsTx *sql.Tx) error {
	jobMetadataExists, err := c.jobMetadataExists(flowJobName)
	if err != nil {
		return fmt.Errorf("failed to get sync status for flow job: %w", err)
	}

	if !jobMetadataExists {
		_, err := syncRecordsTx.ExecContext(c.ctx,
			fmt.Sprintf(insertJobMetadataSQL, peerDBInternalSchema, mirrorJobsTableIdentifier),
			flowJobName, lastCP, syncBatchID, 0)
		if err != nil {
			return fmt.Errorf("failed to insert flow job status: %w", err)
		}
	} else {
		_, err := syncRecordsTx.ExecContext(c.ctx,
			fmt.Sprintf(updateMetadataForSyncRecordsSQL, peerDBInternalSchema, mirrorJobsTableIdentifier),
			lastCP, syncBatchID, flowJobName)
		if err != nil {
			return fmt.Errorf("failed to update flow job status: %w", err)
		}
	}

	return nil
}

func (c *SnowflakeConnector) updateNormalizeMetadata(flowJobName string, normalizeBatchID int64, normalizeRecordsTx *sql.Tx) error {
	jobMetadataExists, err := c.jobMetadataExists(flowJobName)
	if err != nil {
		return fmt.Errorf("failed to get sync status for flow job: %w", err)
	}
	if !jobMetadataExists {
		return fmt.Errorf("job metadata does not exist, unable to update")
	}

	_, err = normalizeRecordsTx.ExecContext(c.ctx,
		fmt.Sprintf(updateMetadataForNormalizeRecordsSQL, peerDBInternalSchema, mirrorJobsTableIdentifier),
		normalizeBatchID, flowJobName)
	if err != nil {
		return fmt.Errorf("failed to update metadata for NormalizeTables: %w", err)
	}

	return nil
}

func (c *SnowflakeConnector) createPeerDBInternalSchema(createsSchemaTx *sql.Tx) error {
	_, err := createsSchemaTx.ExecContext(c.ctx, fmt.Sprintf(createPeerDBInternalSchemaSQL, peerDBInternalSchema))
	if err != nil {
		return fmt.Errorf("error while creating internal schema for PeerDB: %w", err)
	}
	return nil
}