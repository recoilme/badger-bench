package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"testing"

	"github.com/bmatsuo/lmdb-go/lmdb"
	"github.com/dgraph-io/badger"
	"github.com/dgraph-io/badger-bench/store"
	"github.com/dgraph-io/badger/table"
	"github.com/dgraph-io/badger/y"
)

var (
	ctx           = context.Background()
	numKeys       = flag.Float64("keys_mil", 10.0, "How many million keys to write.")
	flagDir       = flag.String("dir", "bench-tmp", "Where data is temporarily stored.")
	flagValueSize = flag.Int("valsz", 128, "Size of each value.")
)

const Mi int = 1000000
const Mf float64 = 1000000

func getBadger() (*badger.KV, error) {
	opt := badger.DefaultOptions
	opt.MapTablesTo = table.LoadToRAM
	opt.Dir = *flagDir + "/badger"
	opt.ValueDir = opt.Dir
	fmt.Println(opt.Dir)
	opt.DoNotCompact = true
	opt.ValueGCThreshold = 0.0
	return badger.NewKV(&opt)
}

func getRocks() *store.Store {
	rdb, err := store.NewReadOnlyStore(*flagDir + "/rocks")
	y.Check(err)
	return rdb
}

func getLmdb() *lmdb.Env {
	lmdbEnv, err := lmdb.NewEnv()
	y.Check(err)
	err = lmdbEnv.SetMaxDBs(1)
	y.Check(err)
	err = lmdbEnv.SetMapSize(1 << 38) // ~273Gb
	y.Check(err)

	err = lmdbEnv.Open(*flagDir+"/lmdb", lmdb.Readonly, 0777)
	y.Check(err)
	return lmdbEnv
}

func newKey() []byte {
	k := rand.Int() % int(*numKeys*Mf)
	key := fmt.Sprintf("vsz=%05d-k=%010d", *flagValueSize, k) // 22 bytes.
	return []byte(key)
}

func print(count int) {
	if count%100000 == 0 {
		fmt.Printf(".")
	} else if count%Mi == 0 {
		fmt.Printf("-")
	}
}

func BenchmarkReadRandomBadger(b *testing.B) {
	bdb, err := getBadger()
	y.Check(err)
	defer bdb.Close()

	b.Run("read-random-badger", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			var count int
			for pb.Next() {
				key := newKey()
				var val badger.KVItem
				if bdb.Get(key, &val); val.Value() != nil {
					count++
				}
			}
			if count > 100000 {
				b.Logf("badger %d keys had valid values.", count)
			}
		})
	})
}

func BenchmarkReadRandomRocks(b *testing.B) {
	rdb := getRocks()
	defer rdb.Close()

	b.Run("read-random-rocks", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			var count int
			for pb.Next() {
				key := newKey()
				if _, err := rdb.Get(key); err == nil {
					count++
				}
			}
			if count > 100000 {
				b.Logf("rocks %d keys had valid values.", count)
			}
		})
	})
}

func BenchmarkReadRandomLmdb(b *testing.B) {
	lmdbEnv := getLmdb()
	defer lmdbEnv.Close()

	var lmdbDBI lmdb.DBI
	// Acquire handle
	err := lmdbEnv.View(func(txn *lmdb.Txn) error {
		var err error
		lmdbDBI, err = txn.OpenDBI("bench", 0)
		return err
	})
	y.Check(err)
	defer lmdbEnv.CloseDBI(lmdbDBI)

	b.Run("read-random-lmdb", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			var count int
			for pb.Next() {
				key := newKey()
				_ = lmdbEnv.View(func(txn *lmdb.Txn) error {
					_, err := txn.Get(lmdbDBI, key)
					if err != nil {
						return err
					}
					count++
					return nil
				})
			}
			if count > 100000 {
				b.Logf("lmdb %d keys had valid values.", count)
			}
		})
	})
}

func safecopy(dst []byte, src []byte) []byte {
	if cap(dst) < len(src) {
		dst = make([]byte, len(src))
	}
	dst = dst[0:len(src)]
	copy(dst, src)
	return dst
}

func BenchmarkIterateRocks(b *testing.B) {
	rdb := getRocks()
	k := make([]byte, 1024)
	v := make([]byte, Mi)
	b.ResetTimer()

	b.Run("rocksdb-iterate", func(b *testing.B) {
		for j := 0; j < b.N; j++ {
			itr := rdb.NewIterator()
			var count int
			for itr.SeekToFirst(); itr.Valid(); itr.Next() {
				{
					// do some processing.
					k = safecopy(k, itr.Key().Data())
					v = safecopy(v, itr.Value().Data())
				}
				count++
				if count > 2*Mi {
					break
				}
			}
			b.Logf("[%d] Counted %d keys\n", j, count)
		}
	})
}

func BenchmarkIterateLmdb(b *testing.B) {
	lmdbEnv := getLmdb()

	var lmdbDBI lmdb.DBI
	// Acquire handle
	err := lmdbEnv.View(func(txn *lmdb.Txn) error {
		var err error
		lmdbDBI, err = txn.OpenDBI("bench", 0)
		return err
	})
	y.Check(err)
	defer lmdbEnv.CloseDBI(lmdbDBI)

	k := make([]byte, 1024)
	v := make([]byte, Mi)
	b.ResetTimer()

	b.Run("lmdb-iterate", func(b *testing.B) {
		for j := 0; j < b.N; j++ {
			var count int
			err = lmdbEnv.View(func(txn *lmdb.Txn) error {
				cur, err := txn.OpenCursor(lmdbDBI)
				if err != nil {
					return err
				}
				defer cur.Close()

				for {
					k1, v1, err := cur.Get(nil, nil, lmdb.Next)
					if lmdb.IsNotFound(err) {
						return nil
					}
					if err != nil {
						return err
					}

					//fmt.Printf("%s %s\n", k, v)

					// do some processing.
					k = safecopy(k, k1)
					v = safecopy(v, v1)

					count++
					if count > 2*Mi {
						break
					}
				}
				return nil
			})
			y.Check(err)
		}
	})
}

func BenchmarkIterateBadgerOnlyKeys(b *testing.B) {
	bdb, err := getBadger()
	y.Check(err)
	k := make([]byte, 1024)
	b.ResetTimer()

	b.Run("badger-iterate-onlykeys", func(b *testing.B) {
		for j := 0; j < b.N; j++ {
			var count int
			// 100 = size, 0 = num workers, false = fwd direction.
			opt := badger.IteratorOptions{}
			opt.PrefetchSize = 10000
			itr := bdb.NewIterator(opt)
			for itr.Rewind(); itr.Valid(); itr.Next() {
				item := itr.Item()
				{
					// do some processing.
					k = safecopy(k, item.Key())
				}
				count++
				if count > 2*Mi {
					break
				}
			}
			b.Logf("[%d] Counted %d keys\n", j, count)
		}
	})
}

func BenchmarkIterateBadgerWithValues(b *testing.B) {
	bdb, err := getBadger()
	y.Check(err)
	k := make([]byte, 1024)
	v := make([]byte, Mi)
	b.ResetTimer()

	b.Run("badger-iterate-withvals", func(b *testing.B) {
		for j := 0; j < b.N; j++ {
			var count int
			opt := badger.IteratorOptions{}
			opt.PrefetchSize = 10000
			opt.FetchValues = true
			itr := bdb.NewIterator(opt)
			for itr.Rewind(); itr.Valid(); itr.Next() {
				item := itr.Item()
				{
					// do some processing.
					k = safecopy(k, item.Key())
					v = safecopy(v, item.Value())
				}
				count++
				print(count)
				if count >= 2*Mi {
					break
				}
			}
			b.Logf("[%d] Counted %d keys\n", j, count)
		}
	})
}

func TestMain(m *testing.M) {
	flag.Parse()
	// call flag.Parse() here if TestMain uses flags
	go http.ListenAndServe(":8080", nil)
	os.Exit(m.Run())
}
