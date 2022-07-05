package mvcc

import (
	"bytes"

	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
)

// Scanner is used for reading multiple sequential key/value pairs from the storage layer. It is aware of the implementation
// of the storage layer and returns results suitable for users.
// Invariant: either the scanner is finished and cannot be used, or it is ready to return a value immediately.
type Scanner struct {
	// Your Data Here (4C).
	txn      *MvccTxn
	nextKey  []byte
	iter     engine_util.DBIterator
	finished bool
}

// NewScanner creates a new scanner ready to read from the snapshot in txn.
func NewScanner(startKey []byte, txn *MvccTxn) *Scanner {
	// Your Code Here (4C).
	scanner := &Scanner{
		nextKey:  startKey,
		txn:      txn,
		iter:     txn.Reader.IterCF(engine_util.CfWrite),
		finished: false,
	}
	return scanner
}

func (scan *Scanner) Close() {
	// Your Code Here (4C).
	scan.iter.Close()
}

// Next returns the next key/value pair from the scanner. If the scanner is exhausted, then it will return `nil, nil, nil`.
func (scan *Scanner) Next() ([]byte, []byte, error) {
	// Your Code Here (4C).
	if scan.finished {
		return nil, nil, nil
	}

	scan.iter.Seek(EncodeKey(scan.nextKey, scan.txn.StartTS))
	if !scan.iter.Valid() {
		scan.finished = true
		return nil, nil, nil
	}
	item := scan.iter.Item()
	itemKey := item.KeyCopy(nil)
	userkey := DecodeUserKey(itemKey)
	if !bytes.Equal(userkey, scan.nextKey) {
		scan.nextKey = userkey
		return scan.Next()
	}
	for {
		scan.iter.Next()
		if !scan.iter.Valid() {
			scan.finished = true
			break
		}
		item := scan.iter.Item()
		itemKey := item.KeyCopy(nil)
		userkey := DecodeUserKey(itemKey)
		if !bytes.Equal(userkey, scan.nextKey) {
			scan.nextKey = userkey
			break
		}
	}
	value, _ := item.ValueCopy(nil)
	write, _ := ParseWrite(value)
	if write.Kind == WriteKindDelete {
		return userkey, nil, nil
	}
	value, err := scan.txn.Reader.GetCF(engine_util.CfDefault, EncodeKey(userkey, write.StartTS))
	if err != nil {
		return userkey, nil, err
	}
	return userkey, value, nil
}
