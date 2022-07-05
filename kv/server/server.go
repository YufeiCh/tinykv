package server

import (
	"context"
	"fmt"

	"github.com/pingcap-incubator/tinykv/kv/coprocessor"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/kv/storage/raft_storage"
	"github.com/pingcap-incubator/tinykv/kv/transaction/latches"
	"github.com/pingcap-incubator/tinykv/kv/transaction/mvcc"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
	coppb "github.com/pingcap-incubator/tinykv/proto/pkg/coprocessor"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/tinykvpb"
	"github.com/pingcap/tidb/kv"
)

var _ tinykvpb.TinyKvServer = new(Server)

// Server is a TinyKV server, it 'faces outwards', sending and receiving messages from clients such as TinySQL.
type Server struct {
	storage storage.Storage

	// (Used in 4A/4B)
	Latches *latches.Latches

	// coprocessor API handler, out of course scope
	copHandler *coprocessor.CopHandler
}

func NewServer(storage storage.Storage) *Server {
	return &Server{
		storage: storage,
		Latches: latches.NewLatches(),
	}
}

// The below functions are Server's gRPC API (implements TinyKvServer).

// Raft commands (tinykv <-> tinykv)
// Only used for RaftStorage, so trivially forward it.
func (server *Server) Raft(stream tinykvpb.TinyKv_RaftServer) error {
	return server.storage.(*raft_storage.RaftStorage).Raft(stream)
}

// Snapshot stream (tinykv <-> tinykv)
// Only used for RaftStorage, so trivially forward it.
func (server *Server) Snapshot(stream tinykvpb.TinyKv_SnapshotServer) error {
	return server.storage.(*raft_storage.RaftStorage).Snapshot(stream)
}

// Transactional API.
func (server *Server) KvGet(_ context.Context, req *kvrpcpb.GetRequest) (*kvrpcpb.GetResponse, error) {
	// Your Code Here (4B).
	getResponse := &kvrpcpb.GetResponse{}
	reader, err := server.storage.Reader(req.GetContext())
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			getResponse.RegionError = regionErr.RequestErr
			return getResponse, nil
		}
		return nil, err
	}
	defer reader.Close()
	txn := mvcc.NewMvccTxn(reader, req.Version)
	lock, err := txn.GetLock(req.Key)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			getResponse.RegionError = regionErr.RequestErr
			return getResponse, nil
		}
		return nil, err
	}
	if lock != nil && lock.Ts <= req.Version {
		getResponse.Error = &kvrpcpb.KeyError{
			Locked: &kvrpcpb.LockInfo{
				PrimaryLock: lock.Primary,
				LockVersion: lock.Ts,
				Key:         req.Key,
				LockTtl:     lock.Ttl,
			},
		}
		return getResponse, nil
	}
	value, err := txn.GetValue(req.Key)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			getResponse.RegionError = regionErr.RequestErr
			return getResponse, nil
		}
		return nil, err
	}
	getResponse.Value = value
	if value == nil {
		getResponse.NotFound = true
	}
	return getResponse, nil
}

func (server *Server) KvPrewrite(_ context.Context, req *kvrpcpb.PrewriteRequest) (*kvrpcpb.PrewriteResponse, error) {
	// Your Code Here (4B).
	response := &kvrpcpb.PrewriteResponse{}
	reader, err := server.storage.Reader(req.GetContext())
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionErr.RequestErr
			return response, nil
		}
		return nil, err
	}
	defer reader.Close()
	txn := mvcc.NewMvccTxn(reader, req.StartVersion)
	var keyError []*kvrpcpb.KeyError
	for _, mutation := range req.Mutations {
		key := mutation.Key
		write, ts, err := txn.MostRecentWrite(key)
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				response.RegionError = regionErr.RequestErr
				return response, nil
			}
			return nil, err
		}
		if write != nil && ts >= req.StartVersion {
			keyError = append(keyError, &kvrpcpb.KeyError{
				Conflict: &kvrpcpb.WriteConflict{
					StartTs:    req.StartVersion,
					ConflictTs: write.StartTS,
					Key:        key,
					Primary:    req.PrimaryLock,
				},
			})
			continue
		}
		lock, err := txn.GetLock(key)
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				response.RegionError = regionErr.RequestErr
				return response, nil
			}
			return nil, err
		}
		if lock != nil && lock.Ts != req.StartVersion {
			keyError = append(keyError, &kvrpcpb.KeyError{
				Locked: &kvrpcpb.LockInfo{
					PrimaryLock: lock.Primary,
					LockVersion: lock.Ts,
					Key:         mutation.Key,
					LockTtl:     lock.Ttl,
				},
			})
			continue
		}
		var kind mvcc.WriteKind
		switch mutation.Op {
		case kvrpcpb.Op_Put:
			txn.PutValue(mutation.Key, mutation.Value)
			kind = mvcc.WriteKindPut
		case kvrpcpb.Op_Del:
			txn.DeleteValue(mutation.Key)
			kind = mvcc.WriteKindDelete
		default:
			return nil, fmt.Errorf("prewrite invalid op")
		}
		txn.PutLock(mutation.Key, &mvcc.Lock{
			Primary: req.PrimaryLock,
			Ts:      req.StartVersion,
			Ttl:     req.LockTtl,
			Kind:    kind,
		})
	}
	if len(keyError) > 0 {
		response.Errors = keyError
		return response, nil
	}
	server.storage.Write(req.Context, txn.Writes())
	return response, nil
}

func (server *Server) KvCommit(_ context.Context, req *kvrpcpb.CommitRequest) (*kvrpcpb.CommitResponse, error) {
	// Your Code Here (4B).
	response := &kvrpcpb.CommitResponse{}
	reader, err := server.storage.Reader(req.GetContext())
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionErr.RequestErr
			return response, nil
		}
		return nil, err
	}
	defer reader.Close()
	txn := mvcc.NewMvccTxn(reader, req.StartVersion)
	server.Latches.WaitForLatches(req.Keys)
	defer server.Latches.ReleaseLatches(req.Keys)
	for _, key := range req.Keys {
		lock, err := txn.GetLock(key)
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				response.RegionError = regionErr.RequestErr
				return response, nil
			}
			return nil, err
		}
		if lock == nil {
			return response, nil
		}
		if lock.Ts != req.StartVersion {
			response.Error = &kvrpcpb.KeyError{
				Retryable: "True",
			}
			return response, nil
		}
		txn.PutWrite(key, req.CommitVersion, &mvcc.Write{
			StartTS: req.StartVersion,
			Kind:    lock.Kind,
		})
		txn.DeleteLock(key)
	}
	err = server.storage.Write(req.Context, txn.Writes())
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionErr.RequestErr
			return response, nil
		}
		return nil, err
	}
	return response, nil
}

func (server *Server) KvScan(_ context.Context, req *kvrpcpb.ScanRequest) (*kvrpcpb.ScanResponse, error) {
	// Your Code Here (4C).
	response := &kvrpcpb.ScanResponse{}
	pairs := make([]*kvrpcpb.KvPair, 0)
	reader, err := server.storage.Reader(req.GetContext())
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionErr.RequestErr
			return response, nil
		}
		return nil, err
	}
	defer reader.Close()
	txn := mvcc.NewMvccTxn(reader, req.Version)
	scanner := mvcc.NewScanner(req.StartKey, txn)
	defer scanner.Close()
	count := 0
	for {
		key, value, err := scanner.Next()
		count++
		if key == nil || count > int(req.Limit) {
			break
		}
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				response.RegionError = regionErr.RequestErr
				return response, nil
			}
		}
		if value != nil {
			pairs = append(pairs, &kvrpcpb.KvPair{Key: key, Value: value})
		}
	}
	response.Pairs = pairs
	return response, nil
}

func (server *Server) KvCheckTxnStatus(_ context.Context, req *kvrpcpb.CheckTxnStatusRequest) (*kvrpcpb.CheckTxnStatusResponse, error) {
	// Your Code Here (4C).
	response := &kvrpcpb.CheckTxnStatusResponse{}
	reader, err := server.storage.Reader(req.GetContext())
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionErr.RequestErr
			return response, nil
		}
		return nil, err
	}
	defer reader.Close()
	txn := mvcc.NewMvccTxn(reader, req.LockTs)
	key := req.PrimaryKey
	write, ts, err := txn.CurrentWrite(key)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionErr.RequestErr
			return response, nil
		}
		return nil, err
	}
	if write != nil {
		if write.Kind != mvcc.WriteKindRollback {
			response.CommitVersion = ts
		}
		return response, nil
	}
	lock, err := txn.GetLock(key)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionErr.RequestErr
			return response, nil
		}
		return nil, err
	}
	if lock == nil {
		response.Action = kvrpcpb.Action_LockNotExistRollback
		txn.PutWrite(key, req.LockTs, &mvcc.Write{StartTS: req.LockTs, Kind: mvcc.WriteKindRollback})
	} else {
		response.LockTtl = lock.Ttl
		if mvcc.PhysicalTime(req.CurrentTs)-mvcc.PhysicalTime(lock.Ts) >= lock.Ttl {
			txn.DeleteValue(key)
			txn.DeleteLock(key)
			txn.PutWrite(key, req.LockTs, &mvcc.Write{StartTS: req.LockTs, Kind: mvcc.WriteKindRollback})
			response.Action = kvrpcpb.Action_TTLExpireRollback
		} else {
			response.Action = kvrpcpb.Action_NoAction
		}
	}
	err = server.storage.Write(req.Context, txn.Writes())
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionErr.RequestErr
			return response, nil
		}
		return nil, err
	}
	return response, nil
}

func (server *Server) KvBatchRollback(_ context.Context, req *kvrpcpb.BatchRollbackRequest) (*kvrpcpb.BatchRollbackResponse, error) {
	// Your Code Here (4C).
	response := &kvrpcpb.BatchRollbackResponse{}
	reader, err := server.storage.Reader(req.GetContext())
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionErr.RequestErr
			return response, nil
		}
		return nil, err
	}
	defer reader.Close()
	txn := mvcc.NewMvccTxn(reader, req.StartVersion)
	server.Latches.WaitForLatches(req.Keys)
	defer server.Latches.ReleaseLatches(req.Keys)
	for _, key := range req.Keys {
		write, _, err := txn.CurrentWrite(key)
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				response.RegionError = regionErr.RequestErr
				return response, nil
			}
			return nil, err
		}
		if write != nil {
			if write.Kind == mvcc.WriteKindRollback {
				continue
			} else {
				response.Error = &kvrpcpb.KeyError{Abort: "true"}
				return response, nil
			}
		}
		lock, err := txn.GetLock(key)
		if err != nil {
			if regionErr, ok := err.(*raft_storage.RegionError); ok {
				response.RegionError = regionErr.RequestErr
				return response, nil
			}
			return nil, err
		}
		txn.PutWrite(key, req.StartVersion, &mvcc.Write{StartTS: req.StartVersion, Kind: mvcc.WriteKindRollback})
		if lock == nil || lock.Ts != req.StartVersion {
			continue
		}
		txn.DeleteLock(key)
		txn.DeleteValue(key)
	}
	err = server.storage.Write(req.Context, txn.Writes())
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionErr.RequestErr
			return response, nil
		}
		return nil, err
	}
	return response, nil
}

func (server *Server) KvResolveLock(_ context.Context, req *kvrpcpb.ResolveLockRequest) (*kvrpcpb.ResolveLockResponse, error) {
	// Your Code Here (4C).
	response := &kvrpcpb.ResolveLockResponse{}
	reader, err := server.storage.Reader(req.GetContext())
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			response.RegionError = regionErr.RequestErr
			return response, nil
		}
		return nil, err
	}
	defer reader.Close()
	var keys [][]byte
	iter := reader.IterCF(engine_util.CfLock)
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		item := iter.Item()
		val, err := item.Value()
		if err != nil {
			return nil, err
		}
		lock, err := mvcc.ParseLock(val)
		if err != nil {
			return response, err
		}
		if lock.Ts == req.StartVersion {
			keys = append(keys, item.KeyCopy(nil))
		}
	}
	if len(keys) == 0 {
		return response, nil
	}
	if req.CommitVersion == 0 {
		responseRollBack, err := server.KvBatchRollback(nil, &kvrpcpb.BatchRollbackRequest{
			Context:      req.Context,
			StartVersion: req.StartVersion,
			Keys:         keys,
		})
		response.Error = responseRollBack.Error
		response.RegionError = responseRollBack.RegionError
		return response, err
	} else {
		responseCommit, err := server.KvCommit(nil, &kvrpcpb.CommitRequest{
			Context:       req.Context,
			StartVersion:  req.StartVersion,
			CommitVersion: req.CommitVersion,
			Keys:          keys,
		})
		response.Error = responseCommit.Error
		response.RegionError = responseCommit.RegionError
		return response, err
	}
}

// SQL push down commands.
func (server *Server) Coprocessor(_ context.Context, req *coppb.Request) (*coppb.Response, error) {
	resp := new(coppb.Response)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	switch req.Tp {
	case kv.ReqTypeDAG:
		return server.copHandler.HandleCopDAGRequest(reader, req), nil
	case kv.ReqTypeAnalyze:
		return server.copHandler.HandleCopAnalyzeRequest(reader, req), nil
	}
	return nil, nil
}
