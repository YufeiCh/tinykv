package server

import (
	"context"

	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
)

// The functions below are Server's Raw API. (implements TinyKvServer).
// Some helper methods can be found in sever.go in the current directory

// RawGet return the corresponding Get response based on RawGetRequest's CF and Key fields
func (server *Server) RawGet(_ context.Context, req *kvrpcpb.RawGetRequest) (*kvrpcpb.RawGetResponse, error) {
	reader, _ := server.storage.Reader(&kvrpcpb.Context{})
	value, err := reader.GetCF(req.Cf, req.Key)
	result := new(kvrpcpb.RawGetResponse)
	result.Value = value
	if value == nil {
		result.NotFound = true
	}
	return result, err
}

// RawPut puts the target data into storage and returns the corresponding response
func (server *Server) RawPut(_ context.Context, req *kvrpcpb.RawPutRequest) (*kvrpcpb.RawPutResponse, error) {
	err := server.storage.Write(&kvrpcpb.Context{}, []storage.Modify{
		{
			Data: storage.Put{
				Cf:    req.Cf,
				Key:   req.Key,
				Value: req.Value,
			},
		},
	})
	return &kvrpcpb.RawPutResponse{}, err
}

// RawDelete delete the target data from storage and returns the corresponding response
func (server *Server) RawDelete(_ context.Context, req *kvrpcpb.RawDeleteRequest) (*kvrpcpb.RawDeleteResponse, error) {
	err := server.storage.Write(&kvrpcpb.Context{}, []storage.Modify{
		{
			Data: storage.Delete{
				Cf:  req.Cf,
				Key: req.Key,
			},
		},
	})
	return &kvrpcpb.RawDeleteResponse{}, err
}

// RawScan scan the data starting from the start key up to limit. and return the corresponding result
func (server *Server) RawScan(_ context.Context, req *kvrpcpb.RawScanRequest) (*kvrpcpb.RawScanResponse, error) {
	reader, err := server.storage.Reader(&kvrpcpb.Context{})
	it := reader.IterCF(req.Cf)
	var kvPairs []*kvrpcpb.KvPair
	it.Seek(req.StartKey)
	for num := 0; it.Valid() && num < int(req.Limit); num++ {
		item := it.Item()
		val, errorValue := item.ValueCopy(nil)
		if errorValue != nil {
			return nil, errorValue
		}
		kvPairs = append(kvPairs, &kvrpcpb.KvPair{
			Key:   item.KeyCopy(nil),
			Value: val,
		})
		it.Next()
	}
	return &kvrpcpb.RawScanResponse{Kvs: kvPairs}, err
}
