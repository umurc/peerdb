package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/PeerDB-io/peer-flow/activities"
	"github.com/PeerDB-io/peer-flow/generated/protos"
	"github.com/PeerDB-io/peer-flow/model"
	"github.com/PeerDB-io/peer-flow/model/qvalue"
	peerflow "github.com/PeerDB-io/peer-flow/workflows"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	log "github.com/sirupsen/logrus"
	"go.temporal.io/sdk/testsuite"
)

// readFileToBytes reads a file to a byte array.
func ReadFileToBytes(path string) ([]byte, error) {
	var ret []byte

	f, err := os.Open(path)
	if err != nil {
		return ret, fmt.Errorf("failed to open file: %w", err)
	}

	defer f.Close()

	ret, err = io.ReadAll(f)
	if err != nil {
		return ret, fmt.Errorf("failed to read file: %w", err)
	}

	return ret, nil
}

func RegisterWorkflowsAndActivities(env *testsuite.TestWorkflowEnvironment) {
	// set a 300 second timeout for the workflow to execute a few runs.
	env.SetTestTimeout(300 * time.Second)

	env.RegisterWorkflow(peerflow.CDCFlowWorkflowWithConfig)
	env.RegisterWorkflow(peerflow.SyncFlowWorkflow)
	env.RegisterWorkflow(peerflow.SetupFlowWorkflow)
	env.RegisterWorkflow(peerflow.SnapshotFlowWorkflow)
	env.RegisterWorkflow(peerflow.NormalizeFlowWorkflow)
	env.RegisterWorkflow(peerflow.QRepFlowWorkflow)
	env.RegisterWorkflow(peerflow.QRepPartitionWorkflow)
	env.RegisterActivity(&activities.FlowableActivity{})
	env.RegisterActivity(&activities.SnapshotActivity{})
}

func SetupCDCFlowStatusQuery(env *testsuite.TestWorkflowEnvironment,
	connectionGen FlowConnectionGenerationConfig) {
	// wait for PeerFlowStatusQuery to finish setup
	// sleep for 5 second to allow the workflow to start
	time.Sleep(5 * time.Second)
	for {
		response, err := env.QueryWorkflow(
			peerflow.CDCFlowStatusQuery,
			connectionGen.FlowJobName,
		)
		if err == nil {
			var state peerflow.CDCFlowState
			err = response.Get(&state)
			if err != nil {
				log.Errorln(err)
			}

			if state.SetupComplete {
				fmt.Println("query indicates setup is complete")
				break
			}
		} else {
			// log the error for informational purposes
			log.Errorln(err)
		}
		time.Sleep(1 * time.Second)
	}
}

func NormalizeFlowCountQuery(env *testsuite.TestWorkflowEnvironment,
	connectionGen FlowConnectionGenerationConfig,
	minCount int) {
	// wait for PeerFlowStatusQuery to finish setup
	// sleep for 5 second to allow the workflow to start
	time.Sleep(5 * time.Second)
	for {
		response, err := env.QueryWorkflow(
			peerflow.CDCFlowStatusQuery,
			connectionGen.FlowJobName,
		)
		if err == nil {
			var state peerflow.CDCFlowState
			err = response.Get(&state)
			if err != nil {
				log.Errorln(err)
			}

			if len(state.NormalizeFlowStatuses) >= minCount {
				fmt.Println("query indicates setup is complete")
				break
			}
		} else {
			// log the error for informational purposes
			log.Errorln(err)
		}
		time.Sleep(1 * time.Second)
	}
}

func CreateSourceTableQRep(pool *pgxpool.Pool, suffix string, tableName string) error {
	tblFields := []string{
		"id UUID NOT NULL PRIMARY KEY",
		"card_id UUID",
		`"from" TIMESTAMP NOT NULL`,
		"price NUMERIC",
		"created_at TIMESTAMP NOT NULL",
		"updated_at TIMESTAMP NOT NULL",
		"transaction_hash BYTEA",
		"ownerable_type VARCHAR",
		"ownerable_id UUID",
		"user_nonce INTEGER",
		"transfer_type INTEGER DEFAULT 0 NOT NULL",
		"blockchain INTEGER NOT NULL",
		"deal_type VARCHAR",
		"deal_id UUID",
		"ethereum_transaction_id UUID",
		"ignore_price BOOLEAN DEFAULT false",
		"card_eth_value DOUBLE PRECISION",
		"paid_eth_price DOUBLE PRECISION",
		"card_bought_notified BOOLEAN DEFAULT false NOT NULL",
		"address NUMERIC",
		"account_id UUID",
		"asset_id NUMERIC NOT NULL",
		"status INTEGER",
		"transaction_id UUID",
		"settled_at TIMESTAMP",
		"reference_id VARCHAR",
		"settle_at TIMESTAMP",
		"settlement_delay_reason INTEGER",
		"f1 text[]",
		"f2 bigint[]",
		"f3 int[]",
		"f4 varchar[]",
		"f5 jsonb",
		"f6 jsonb",
		"f7 jsonb",
		"f8 smallint",
	}
	if strings.Contains(tableName, "sf") {
		tblFields = append(tblFields, "geometry_point geometry(point)",
			"geography_point geography(point)",
			"geometry_linestring geometry(linestring)",
			"geography_linestring geography(linestring)",
			"geometry_polygon geometry(polygon)",
			"geography_polygon geography(polygon)")
	}
	tblFieldStr := strings.Join(tblFields, ",")

	_, err := pool.Exec(context.Background(), fmt.Sprintf(`
		CREATE TABLE e2e_test_%s.%s (
			%s
		);`, suffix, tableName, tblFieldStr))
	if err != nil {
		return err
	}

	fmt.Printf("created table on postgres: e2e_test_%s.%s\n", suffix, tableName)
	return nil
}

func generate20MBJson() ([]byte, error) {
	xn := make(map[string]interface{})
	for i := 0; i < 215000; i++ {
		xn[uuid.New().String()] = uuid.New().String()
	}

	v, err := json.Marshal(xn)
	if err != nil {
		return nil, err
	}

	return v, nil
}

func PopulateSourceTable(pool *pgxpool.Pool, suffix string, tableName string, rowCount int) error {
	var ids []string
	var rows []string
	for i := 0; i < rowCount-1; i++ {
		id := uuid.New().String()
		ids = append(ids, id)
		geoValues := ""
		if strings.Contains(tableName, "sf") {
			geoValues = `,'POINT(1 2)','POINT(40.7128 -74.0060)',
			'LINESTRING(0 0, 1 1, 2 2)',
			'LINESTRING(-74.0060 40.7128, -73.9352 40.7306, -73.9123 40.7831)',
			'POLYGON((0 0, 0 1, 1 1, 1 0, 0 0))','POLYGON((0 0, 0 1, 1 1, 1 0, 0 0))'`
		}
		row := fmt.Sprintf(`
					(
							'%s', '%s', CURRENT_TIMESTAMP, 3.86487206688919, CURRENT_TIMESTAMP,
							CURRENT_TIMESTAMP, E'\\\\xDEADBEEF', 'type1', '%s',
							1, 0, 1, 'dealType1',
							'%s', '%s', false, 1.2345,
							1.2345, false, 12345, '%s',
							12345, 1, '%s', CURRENT_TIMESTAMP, 'refID',
							CURRENT_TIMESTAMP, 1, ARRAY['text1', 'text2'], ARRAY[123, 456], ARRAY[789, 012],
							ARRAY['varchar1', 'varchar2'], '{"key": 8.5}',
							'[{"key1": "value1", "key2": "value2", "key3": "value3"}]',
							'{"key": "value"}', 15 %s
					)`,
			id, uuid.New().String(), uuid.New().String(),
			uuid.New().String(), uuid.New().String(), uuid.New().String(), uuid.New().String(), geoValues)
		rows = append(rows, row)
	}

	geoColumns := ""
	if strings.Contains(tableName, "sf") {
		geoColumns = ",geometry_point, geography_point," +
			"geometry_linestring, geography_linestring," +
			"geometry_polygon, geography_polygon"
	}
	_, err := pool.Exec(context.Background(), fmt.Sprintf(`
			INSERT INTO e2e_test_%s.%s (
					id, card_id, "from", price, created_at,
					updated_at, transaction_hash, ownerable_type, ownerable_id,
					user_nonce, transfer_type, blockchain, deal_type,
					deal_id, ethereum_transaction_id, ignore_price, card_eth_value,
					paid_eth_price, card_bought_notified, address, account_id,
					asset_id, status, transaction_id, settled_at, reference_id,
					settle_at, settlement_delay_reason, f1, f2, f3, f4, f5, f6, f7, f8 
					%s
			) VALUES %s;
	`, suffix, tableName, geoColumns, strings.Join(rows, ",")))
	if err != nil {
		return err
	}

	// add a row where all the nullable fields are null
	_, err = pool.Exec(context.Background(), fmt.Sprintf(`
	INSERT INTO e2e_test_%s.%s (
			id, "from", created_at, updated_at,
			transfer_type, blockchain, card_bought_notified, asset_id
	) VALUES (
			'%s', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP,
			0, 1, false, 12345
	);
	`, suffix, tableName, uuid.New().String()))
	if err != nil {
		return err
	}

	// generate a 20 MB json and update id[0]'s col f5 to it
	v, err := generate20MBJson()
	if err != nil {
		return err
	}
	_, err = pool.Exec(context.Background(), fmt.Sprintf(`
		UPDATE e2e_test_%s.%s SET f5 = $1 WHERE id = $2;
	`, suffix, tableName), v, ids[0])
	if err != nil {
		return err
	}

	return nil
}

func CreateQRepWorkflowConfig(
	flowJobName string,
	sourceTable string,
	dstTable string,
	query string,
	syncMode protos.QRepSyncMode,
	dest *protos.Peer,
	stagingPath string,
) (*protos.QRepConfig, error) {
	connectionGen := QRepFlowConnectionGenerationConfig{
		FlowJobName:                flowJobName,
		WatermarkTable:             sourceTable,
		DestinationTableIdentifier: dstTable,
		PostgresPort:               PostgresPort,
		Destination:                dest,
		StagingPath:                stagingPath,
	}

	watermark := "updated_at"

	qrepConfig, err := connectionGen.GenerateQRepConfig(query, watermark, syncMode)
	if err != nil {
		return nil, err
	}

	qrepConfig.InitialCopyOnly = true

	return qrepConfig, nil
}

func RunQrepFlowWorkflow(env *testsuite.TestWorkflowEnvironment, config *protos.QRepConfig) {
	lastPartition := &protos.QRepPartition{
		PartitionId: "not-applicable-partition",
		Range:       nil,
	}
	numPartitionsProcessed := 0
	env.ExecuteWorkflow(peerflow.QRepFlowWorkflow, config, lastPartition, numPartitionsProcessed)
}

func GetOwnersSchema() *model.QRecordSchema {
	return &model.QRecordSchema{
		Fields: []*model.QField{
			{Name: "id", Type: qvalue.QValueKindString, Nullable: true},
			{Name: "card_id", Type: qvalue.QValueKindString, Nullable: true},
			{Name: "from", Type: qvalue.QValueKindTimestamp, Nullable: true},
			{Name: "price", Type: qvalue.QValueKindNumeric, Nullable: true},
			{Name: "created_at", Type: qvalue.QValueKindTimestamp, Nullable: true},
			{Name: "updated_at", Type: qvalue.QValueKindTimestamp, Nullable: true},
			{Name: "transaction_hash", Type: qvalue.QValueKindBytes, Nullable: true},
			{Name: "ownerable_type", Type: qvalue.QValueKindString, Nullable: true},
			{Name: "ownerable_id", Type: qvalue.QValueKindString, Nullable: true},
			{Name: "user_nonce", Type: qvalue.QValueKindInt64, Nullable: true},
			{Name: "transfer_type", Type: qvalue.QValueKindInt64, Nullable: true},
			{Name: "blockchain", Type: qvalue.QValueKindInt64, Nullable: true},
			{Name: "deal_type", Type: qvalue.QValueKindString, Nullable: true},
			{Name: "deal_id", Type: qvalue.QValueKindString, Nullable: true},
			{Name: "ethereum_transaction_id", Type: qvalue.QValueKindString, Nullable: true},
			{Name: "ignore_price", Type: qvalue.QValueKindBoolean, Nullable: true},
			{Name: "card_eth_value", Type: qvalue.QValueKindFloat64, Nullable: true},
			{Name: "paid_eth_price", Type: qvalue.QValueKindFloat64, Nullable: true},
			{Name: "card_bought_notified", Type: qvalue.QValueKindBoolean, Nullable: true},
			{Name: "address", Type: qvalue.QValueKindNumeric, Nullable: true},
			{Name: "account_id", Type: qvalue.QValueKindString, Nullable: true},
			{Name: "asset_id", Type: qvalue.QValueKindNumeric, Nullable: true},
			{Name: "status", Type: qvalue.QValueKindInt64, Nullable: true},
			{Name: "transaction_id", Type: qvalue.QValueKindString, Nullable: true},
			{Name: "settled_at", Type: qvalue.QValueKindTimestamp, Nullable: true},
			{Name: "reference_id", Type: qvalue.QValueKindString, Nullable: true},
			{Name: "settle_at", Type: qvalue.QValueKindTimestamp, Nullable: true},
			{Name: "settlement_delay_reason", Type: qvalue.QValueKindInt64, Nullable: true},
			{Name: "f1", Type: qvalue.QValueKindArrayString, Nullable: true},
			{Name: "f2", Type: qvalue.QValueKindArrayInt64, Nullable: true},
			{Name: "f3", Type: qvalue.QValueKindArrayInt32, Nullable: true},
			{Name: "f4", Type: qvalue.QValueKindArrayString, Nullable: true},
			{Name: "f5", Type: qvalue.QValueKindJSON, Nullable: true},
			{Name: "f6", Type: qvalue.QValueKindJSON, Nullable: true},
			{Name: "f7", Type: qvalue.QValueKindJSON, Nullable: true},
			{Name: "f8", Type: qvalue.QValueKindInt16, Nullable: true},
		},
	}
}

func GetOwnersSelectorString() string {
	schema := GetOwnersSchema()
	var fields []string
	for _, field := range schema.Fields {
		// append quoted field name
		fields = append(fields, fmt.Sprintf(`"%s"`, field.Name))
	}
	return strings.Join(fields, ",")
}
