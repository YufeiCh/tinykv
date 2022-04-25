package standalone_storage

import (
	"github.com/Connor1996/badger"
	"github.com/pingcap-incubator/tinykv/kv/config"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
)

// StandAloneStorage is an implementation of `Storage` for a single-node TinyKV instance. It does not
// communicate with other nodes and all data is stored locally.
type StandAloneStorage struct {
	conf   *config.Config
	engine *engine_util.Engines
}

func NewStandAloneStorage(conf *config.Config) *StandAloneStorage {
	return &StandAloneStorage{
		conf, nil,
	}
}

func (s *StandAloneStorage) Start() error {
	engine := engine_util.NewEngines(
		engine_util.CreateDB(s.conf.DBPath, false),
		nil,
		"",
		"",
	)
	s.engine = engine
	return nil
}

func (s *StandAloneStorage) Stop() error {
	return s.engine.Kv.Close()
}

func (s *StandAloneStorage) Reader(ctx *kvrpcpb.Context) (storage.StorageReader, error) {
	return &StandaloneReader{s.engine.Kv.NewTransaction(false)}, nil
}

func (s *StandAloneStorage) Write(ctx *kvrpcpb.Context, batch []storage.Modify) error {
	for _, m := range batch {
		switch data := m.Data.(type) {
		case storage.Put:
			err := engine_util.PutCF(s.engine.Kv, data.Cf, data.Key, data.Value)
			if err != nil {
				return err
			}
		case storage.Delete:
			err := engine_util.DeleteCF(s.engine.Kv, data.Cf, data.Key)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

type StandaloneReader struct {
	txn *badger.Txn
}

func (s *StandaloneReader) GetCF(cf string, key []byte) (value []byte, err error) {
	value, err = engine_util.GetCFFromTxn(s.txn, cf, key)
	if err == badger.ErrKeyNotFound {
		return nil, nil
	}
	return
}

func (s *StandaloneReader) IterCF(cf string) engine_util.DBIterator {
	return engine_util.NewCFIterator(cf, s.txn)
}

func (s *StandaloneReader) Close() {
	s.txn.Discard()
}
