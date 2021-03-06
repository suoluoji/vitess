// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package vtgate provides query routing rpc services
// for vttablets.
package vtgate

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	mproto "github.com/youtube/vitess/go/mysql/proto"
	"github.com/youtube/vitess/go/vt/key"
	tproto "github.com/youtube/vitess/go/vt/tabletserver/proto"
	"github.com/youtube/vitess/go/vt/tabletserver/tabletconn"
	"github.com/youtube/vitess/go/vt/topo"
	"github.com/youtube/vitess/go/vt/vtgate/proto"
)

// Resolver is the layer to resolve KeyspaceIds and KeyRanges
// to shards. It will try to re-resolve shards if ScatterConn
// returns retryable error, which may imply horizontal or vertical
// resharding happened.
type Resolver struct {
	scatterConn *ScatterConn
}

// NewResolver creates a new Resolver. All input parameters are passed through
// for creating ScatterConn.
func NewResolver(serv SrvTopoServer, cell string, retryDelay time.Duration, retryCount int, timeout time.Duration) *Resolver {
	return &Resolver{
		scatterConn: NewScatterConn(serv, cell, retryDelay, retryCount, timeout),
	}
}

// ExecuteKeyspaceIds executes a non-streaming query based on KeyspaceIds.
// It retries query if new keyspace/shards are re-resolved after a retryable error.
func (res *Resolver) ExecuteKeyspaceIds(context interface{}, query *proto.KeyspaceIdQuery) (*mproto.QueryResult, error) {
	mapToShards := func(keyspace string) ([]string, error) {
		return mapKeyspaceIdsToShards(
			res.scatterConn.toposerv,
			res.scatterConn.cell,
			keyspace,
			query.TabletType,
			query.KeyspaceIds)
	}
	return res.Execute(context, query.Sql, query.BindVariables, query.Keyspace, query.TabletType, query.Session, mapToShards)
}

// ExecuteKeyRanges executes a non-streaming query based on KeyRanges.
// It retries query if new keyspace/shards are re-resolved after a retryable error.
func (res *Resolver) ExecuteKeyRanges(context interface{}, query *proto.KeyRangeQuery) (*mproto.QueryResult, error) {
	mapToShards := func(keyspace string) ([]string, error) {
		return mapKeyRangesToShards(
			res.scatterConn.toposerv,
			res.scatterConn.cell,
			keyspace,
			query.TabletType,
			query.KeyRanges)
	}
	return res.Execute(context, query.Sql, query.BindVariables, query.Keyspace, query.TabletType, query.Session, mapToShards)
}

// Execute executes a non-streaming query based on shards resolved by given func.
// It retries query if new keyspace/shards are re-resolved after a retryable error.
func (res *Resolver) Execute(
	context interface{},
	sql string,
	bindVars map[string]interface{},
	keyspace string,
	tabletType topo.TabletType,
	session *proto.Session,
	mapToShards func(string) ([]string, error),
) (*mproto.QueryResult, error) {
	shards, err := mapToShards(keyspace)
	if err != nil {
		return nil, err
	}
	for {
		qr, err := res.scatterConn.Execute(
			context,
			sql,
			bindVars,
			keyspace,
			shards,
			tabletType,
			NewSafeSession(session))
		if connError, ok := err.(*ShardConnError); ok && connError.Code == tabletconn.ERR_RETRY {
			resharding := false
			// check keyspace change for vertical resharding
			newKeyspace, err := getKeyspaceAlias(
				res.scatterConn.toposerv,
				res.scatterConn.cell,
				keyspace,
				tabletType)
			if err == nil && newKeyspace != keyspace {
				keyspace = newKeyspace
				resharding = true
			}
			// check shards change for horizontal resharding
			newShards, err := mapToShards(keyspace)
			if err != nil {
				return nil, err
			}
			if !StrsEquals(newShards, shards) {
				shards = newShards
				resharding = true
			}
			// retry if resharding happened
			if resharding {
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		return qr, err
	}
}

// ExecuteEntityIds executes a non-streaming query based on given KeyspaceId map.
// It retries query if new keyspace/shards are re-resolved after a retryable error.
func (res *Resolver) ExecuteEntityIds(
	context interface{},
	query *proto.EntityIdsQuery,
) (*mproto.QueryResult, error) {
	shardMap, err := mapEntityIdsToShards(
		res.scatterConn.toposerv,
		res.scatterConn.cell,
		query.Keyspace,
		query.EntityKeyspaceIdMap,
		query.TabletType)
	if err != nil {
		return nil, err
	}
	shards, sqls, bindVars := buildEntityIds(shardMap, query.Sql, query.EntityColumnName, query.BindVariables)
	for {
		qr, err := res.scatterConn.ExecuteEntityIds(
			context,
			shards,
			sqls,
			bindVars,
			query.Keyspace,
			query.TabletType,
			NewSafeSession(query.Session))
		if connError, ok := err.(*ShardConnError); ok && connError.Code == tabletconn.ERR_RETRY {
			resharding := false
			// check keyspace change for vertical resharding
			newKeyspace, err := getKeyspaceAlias(
				res.scatterConn.toposerv,
				res.scatterConn.cell,
				query.Keyspace,
				query.TabletType)
			if err == nil && newKeyspace != query.Keyspace {
				query.Keyspace = newKeyspace
				resharding = true
			}
			// check shards change for horizontal resharding
			newShardMap, err := mapEntityIdsToShards(
				res.scatterConn.toposerv,
				res.scatterConn.cell,
				query.Keyspace,
				query.EntityKeyspaceIdMap,
				query.TabletType)
			if err != nil {
				return nil, err
			}
			newShards, newSqls, newBindVars := buildEntityIds(newShardMap, query.Sql, query.EntityColumnName, query.BindVariables)
			if !StrsEquals(newShards, shards) {
				shards = newShards
				sqls = newSqls
				bindVars = newBindVars
				resharding = true
			}
			// retry if resharding happened
			if resharding {
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		return qr, err
	}
}

// ExecuteBatchKeyspaceIds executes a group of queries based on KeyspaceIds.
// It retries query if new keyspace/shards are re-resolved after a retryable error.
func (res *Resolver) ExecuteBatchKeyspaceIds(context interface{}, query *proto.KeyspaceIdBatchQuery) (*tproto.QueryResultList, error) {
	mapToShards := func(keyspace string) ([]string, error) {
		return mapKeyspaceIdsToShards(
			res.scatterConn.toposerv,
			res.scatterConn.cell,
			keyspace,
			query.TabletType,
			query.KeyspaceIds)
	}
	return res.ExecuteBatch(context, query.Queries, query.Keyspace, query.TabletType, query.Session, mapToShards)
}

// ExecuteBatch executes a group of queries based on shards resolved by given func.
// It retries query if new keyspace/shards are re-resolved after a retryable error.
func (res *Resolver) ExecuteBatch(
	context interface{},
	queries []tproto.BoundQuery,
	keyspace string,
	tabletType topo.TabletType,
	session *proto.Session,
	mapToShards func(string) ([]string, error),
) (*tproto.QueryResultList, error) {
	shards, err := mapToShards(keyspace)
	if err != nil {
		return nil, err
	}
	for {
		qrs, err := res.scatterConn.ExecuteBatch(
			context,
			queries,
			keyspace,
			shards,
			tabletType,
			NewSafeSession(session))
		if connError, ok := err.(*ShardConnError); ok && connError.Code == tabletconn.ERR_RETRY {
			resharding := false
			// check keyspace change for vertical resharding
			newKeyspace, err := getKeyspaceAlias(
				res.scatterConn.toposerv,
				res.scatterConn.cell,
				keyspace,
				tabletType)
			if err == nil && newKeyspace != keyspace {
				keyspace = newKeyspace
				resharding = true
			}
			// check shards change for horizontal resharding
			newShards, err := mapToShards(keyspace)
			if err != nil {
				return nil, err
			}
			if !StrsEquals(newShards, shards) {
				shards = newShards
				resharding = true
			}
			// retry if resharding happened
			if resharding {
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		return qrs, err
	}
}

// StreamExecuteKeyspaceIds executes a streaming query on the specified KeyspaceIds.
// The KeyspaceIds are resolved to shards using the serving graph.
// This function currently temporarily enforces the restriction of executing on
// one shard since it cannot merge-sort the results to guarantee ordering of
// response which is needed for checkpointing.
// The api supports supplying multiple KeyspaceIds to make it future proof.
func (res *Resolver) StreamExecuteKeyspaceIds(context interface{}, query *proto.KeyspaceIdQuery, sendReply func(*mproto.QueryResult) error) error {
	mapToShards := func(keyspace string) ([]string, error) {
		return mapKeyspaceIdsToShards(
			res.scatterConn.toposerv,
			res.scatterConn.cell,
			query.Keyspace,
			query.TabletType,
			query.KeyspaceIds)
	}
	return res.StreamExecute(context, query.Sql, query.BindVariables, query.Keyspace, query.TabletType, query.Session, mapToShards, sendReply)
}

// StreamExecuteKeyRanges executes a streaming query on the specified KeyRanges.
// The KeyRanges are resolved to shards using the serving graph.
// This function currently temporarily enforces the restriction of executing on
// one shard since it cannot merge-sort the results to guarantee ordering of
// response which is needed for checkpointing.
// The api supports supplying multiple keyranges to make it future proof.
func (res *Resolver) StreamExecuteKeyRanges(context interface{}, query *proto.KeyRangeQuery, sendReply func(*mproto.QueryResult) error) error {
	mapToShards := func(keyspace string) ([]string, error) {
		return mapKeyRangesToShards(
			res.scatterConn.toposerv,
			res.scatterConn.cell,
			query.Keyspace,
			query.TabletType,
			query.KeyRanges)
	}
	return res.StreamExecute(context, query.Sql, query.BindVariables, query.Keyspace, query.TabletType, query.Session, mapToShards, sendReply)
}

// StreamExecuteShard executes a streaming query on shards resolved by given func.
// This function currently temporarily enforces the restriction of executing on
// one shard since it cannot merge-sort the results to guarantee ordering of
// response which is needed for checkpointing.
func (res *Resolver) StreamExecute(
	context interface{},
	sql string,
	bindVars map[string]interface{},
	keyspace string,
	tabletType topo.TabletType,
	session *proto.Session,
	mapToShards func(string) ([]string, error),
	sendReply func(*mproto.QueryResult) error,
) error {
	shards, err := mapToShards(keyspace)
	if err != nil {
		return err
	}
	if len(shards) != 1 {
		return fmt.Errorf("Resolved to more than one shard")
	}
	err = res.scatterConn.StreamExecute(
		context,
		sql,
		bindVars,
		keyspace,
		shards,
		tabletType,
		NewSafeSession(session),
		sendReply)
	return err
}

// Commit commits a transaction.
func (res *Resolver) Commit(context interface{}, inSession *proto.Session) error {
	return res.scatterConn.Commit(context, NewSafeSession(inSession))
}

// Rollback rolls back a transaction.
func (res *Resolver) Rollback(context interface{}, inSession *proto.Session) error {
	return res.scatterConn.Rollback(context, NewSafeSession(inSession))
}

// StrsEquals compares contents of two string slices.
func StrsEquals(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Strings(a)
	sort.Strings(b)
	for i, v := range a {
		if v != b[i] {
			return false
		}
	}
	return true
}

func buildEntityIds(shardMap map[string][]key.KeyspaceId, qSql, entityColName string, qBindVars map[string]interface{}) ([]string, map[string]string, map[string]map[string]interface{}) {
	shards := make([]string, 0, 1)
	sqls := make(map[string]string)
	bindVars := make(map[string]map[string]interface{})
	for shard, kids := range shardMap {
		var b bytes.Buffer
		b.Write([]byte(entityColName))
		b.Write([]byte(" in ("))
		bindVar := make(map[string]interface{})
		for k, v := range qBindVars {
			bindVar[k] = v
		}
		for i, kid := range kids {
			bvName := fmt.Sprintf("%v%v", entityColName, i)
			bindVar[bvName] = kid
			if i > 0 {
				b.Write([]byte(", "))
			}
			b.Write([]byte(fmt.Sprintf(":%v", bvName)))
		}
		b.Write([]byte(")"))
		shards = append(shards, shard)
		sqls[shard] = insertSqlClause(qSql, b.String())
		bindVars[shard] = bindVar
	}
	return shards, sqls, bindVars
}

func insertSqlClause(querySql, clause string) string {
	// get first index of any additional clause: group by, order by, limit, for update, sql end if nothing
	// insert clause into the index position
	sql := strings.ToLower(querySql)
	idxExtra := len(sql)
	if idxGroupBy := strings.Index(sql, " group by"); idxGroupBy > 0 && idxGroupBy < idxExtra {
		idxExtra = idxGroupBy
	}
	if idxOrderBy := strings.Index(sql, " order by"); idxOrderBy > 0 && idxOrderBy < idxExtra {
		idxExtra = idxOrderBy
	}
	if idxLimit := strings.Index(sql, " limit"); idxLimit > 0 && idxLimit < idxExtra {
		idxExtra = idxLimit
	}
	if idxForUpdate := strings.Index(sql, " for update"); idxForUpdate > 0 && idxForUpdate < idxExtra {
		idxExtra = idxForUpdate
	}
	var b bytes.Buffer
	b.Write([]byte(querySql[:idxExtra]))
	if strings.Contains(sql, "where") {
		b.Write([]byte(" and "))
	} else {
		b.Write([]byte(" where "))
	}
	b.Write([]byte(clause))
	if idxExtra < len(sql) {
		b.Write([]byte(querySql[idxExtra:]))
	}
	return b.String()
}
