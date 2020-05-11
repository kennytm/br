// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package checksum

import (
	"context"
	"log"

	"github.com/gogo/protobuf/proto"
	"github.com/pingcap/errors"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/tidb/distsql"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/util/ranger"
	"github.com/pingcap/tipb/go-tipb"
	"go.uber.org/zap"

	"github.com/pingcap/br/pkg/utils"
)

// ExecutorBuilder is used to build a "kv.Request".
type ExecutorBuilder struct {
	table *model.TableInfo
	ts    uint64

	oldTable *utils.Table
}

// NewExecutorBuilder returns a new executor builder.
func NewExecutorBuilder(table *model.TableInfo, ts uint64) *ExecutorBuilder {
	return &ExecutorBuilder{
		table: table,
		ts:    ts,
	}
}

// SetOldTable set a old table info to the builder.
func (builder *ExecutorBuilder) SetOldTable(oldTable *utils.Table) *ExecutorBuilder {
	builder.oldTable = oldTable
	return builder
}

// Build builds a checksum executor.
func (builder *ExecutorBuilder) Build() (*Executor, error) {
	reqs, err := buildChecksumRequest(builder.table, builder.oldTable, builder.ts)
	if err != nil {
		return nil, err
	}
	return &Executor{reqs: reqs}, nil
}

func buildChecksumRequest(
	newTable *model.TableInfo,
	oldTable *utils.Table,
	startTS uint64,
) ([]*kv.Request, error) {
	var partDefs []model.PartitionDefinition
	if part := newTable.Partition; part != nil {
		partDefs = part.Definitions
	}

	reqs := make([]*kv.Request, 0, (len(newTable.Indices)+1)*(len(partDefs)+1))
	var oldTableID int64
	if oldTable != nil {
		oldTableID = oldTable.Info.ID
	}
	rs, err := buildRequest(newTable, newTable.ID, oldTable, oldTableID, startTS)
	if err != nil {
		return nil, err
	}
	reqs = append(reqs, rs...)

	for _, partDef := range partDefs {
		var oldPartID int64
		if oldTable != nil {
			for _, oldPartDef := range oldTable.Info.Partition.Definitions {
				if oldPartDef.Name == partDef.Name {
					oldPartID = oldPartDef.ID
				}
			}
		}
		rs, err := buildRequest(newTable, partDef.ID, oldTable, oldPartID, startTS)
		if err != nil {
			return nil, errors.Trace(err)
		}
		reqs = append(reqs, rs...)
	}

	return reqs, nil
}

func buildRequest(
	tableInfo *model.TableInfo,
	tableID int64,
	oldTable *utils.Table,
	oldTableID int64,
	startTS uint64,
) ([]*kv.Request, error) {
	reqs := make([]*kv.Request, 0)
	req, err := buildTableRequest(tableID, oldTable, oldTableID, startTS)
	if err != nil {
		return nil, err
	}
	reqs = append(reqs, req)

	for _, indexInfo := range tableInfo.Indices {
		if indexInfo.State != model.StatePublic {
			continue
		}
		var oldIndexInfo *model.IndexInfo
		if oldTable != nil {
			for _, oldIndex := range oldTable.Info.Indices {
				if oldIndex.Name == indexInfo.Name {
					oldIndexInfo = oldIndex
					break
				}
			}
			if oldIndexInfo == nil {
				log.Panic("index not found",
					zap.Reflect("table", tableInfo),
					zap.Reflect("oldTable", oldTable.Info),
					zap.Stringer("index", indexInfo.Name))
			}
		}
		req, err = buildIndexRequest(
			tableID, indexInfo, oldTableID, oldIndexInfo, startTS)
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, req)
	}

	return reqs, nil
}

func buildTableRequest(
	tableID int64,
	oldTable *utils.Table,
	oldTableID int64,
	startTS uint64,
) (*kv.Request, error) {
	var rule *tipb.ChecksumRewriteRule
	if oldTable != nil {
		rule = &tipb.ChecksumRewriteRule{
			OldPrefix: tablecodec.GenTableRecordPrefix(oldTableID),
			NewPrefix: tablecodec.GenTableRecordPrefix(tableID),
		}
	}

	checksum := &tipb.ChecksumRequest{
		ScanOn:    tipb.ChecksumScanOn_Table,
		Algorithm: tipb.ChecksumAlgorithm_Crc64_Xor,
		Rule:      rule,
	}

	ranges := ranger.FullIntRange(false)

	var builder distsql.RequestBuilder
	// Use low priority to reducing impact to other requests.
	builder.Request.Priority = kv.PriorityLow
	return builder.SetTableRanges(tableID, ranges, nil).
		SetStartTS(startTS).
		SetChecksumRequest(checksum).
		SetConcurrency(variable.DefDistSQLScanConcurrency).
		Build()
}

func buildIndexRequest(
	tableID int64,
	indexInfo *model.IndexInfo,
	oldTableID int64,
	oldIndexInfo *model.IndexInfo,
	startTS uint64,
) (*kv.Request, error) {
	var rule *tipb.ChecksumRewriteRule
	if oldIndexInfo != nil {
		rule = &tipb.ChecksumRewriteRule{
			OldPrefix: tablecodec.EncodeTableIndexPrefix(oldTableID, oldIndexInfo.ID),
			NewPrefix: tablecodec.EncodeTableIndexPrefix(tableID, indexInfo.ID),
		}
	}
	checksum := &tipb.ChecksumRequest{
		ScanOn:    tipb.ChecksumScanOn_Index,
		Algorithm: tipb.ChecksumAlgorithm_Crc64_Xor,
		Rule:      rule,
	}

	ranges := ranger.FullRange()

	var builder distsql.RequestBuilder
	// Use low priority to reducing impact to other requests.
	builder.Request.Priority = kv.PriorityLow
	return builder.SetIndexRanges(nil, tableID, indexInfo.ID, ranges).
		SetStartTS(startTS).
		SetChecksumRequest(checksum).
		SetConcurrency(variable.DefDistSQLScanConcurrency).
		Build()
}

func sendChecksumRequest(
	ctx context.Context,
	client kv.Client,
	req *kv.Request,
) (resp *tipb.ChecksumResponse, err error) {
	res, err := distsql.Checksum(ctx, client, req, nil)
	if err != nil {
		return nil, err
	}
	res.Fetch(ctx)
	defer func() {
		if err1 := res.Close(); err1 != nil {
			err = err1
		}
	}()

	resp = &tipb.ChecksumResponse{}

	for {
		data, err := res.NextRaw(ctx)
		if err != nil {
			return nil, err
		}
		if data == nil {
			break
		}
		checksum := &tipb.ChecksumResponse{}
		if err = checksum.Unmarshal(data); err != nil {
			return nil, err
		}
		updateChecksumResponse(resp, checksum)
	}

	return resp, nil
}

func updateChecksumResponse(resp, update *tipb.ChecksumResponse) {
	resp.Checksum ^= update.Checksum
	resp.TotalKvs += update.TotalKvs
	resp.TotalBytes += update.TotalBytes
}

// Executor is a checksum executor.
type Executor struct {
	reqs []*kv.Request
}

// Len returns the total number of checksum requests.
func (exec *Executor) Len() int {
	return len(exec.reqs)
}

// RawRequests extracts the raw requests associated with this executor.
// This is mainly used for debugging only.
func (exec *Executor) RawRequests() ([]*tipb.ChecksumRequest, error) {
	res := make([]*tipb.ChecksumRequest, 0, len(exec.reqs))
	for _, req := range exec.reqs {
		rawReq := new(tipb.ChecksumRequest)
		if err := proto.Unmarshal(req.Data, rawReq); err != nil {
			return nil, err
		}
		res = append(res, rawReq)
	}
	return res, nil
}

// Execute executes a checksum executor.
func (exec *Executor) Execute(
	ctx context.Context,
	client kv.Client,
	updateFn func(),
) (*tipb.ChecksumResponse, error) {
	checksumResp := &tipb.ChecksumResponse{}
	for _, req := range exec.reqs {
		resp, err := sendChecksumRequest(ctx, client, req)
		if err != nil {
			return nil, err
		}
		updateChecksumResponse(checksumResp, resp)
		updateFn()
	}
	return checksumResp, nil
}
