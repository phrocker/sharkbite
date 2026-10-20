// generate.go writes a two-tablet WAL using Sharkbite's native encoder.
// It is intentionally separate from the production WAL publication path.
package main

import (
	"fmt"
	"os"

	"github.com/phrocker/shoal-oss/internal/qwal"
)

func main() {
	if len(os.Args) != 2 {
		panic("usage: go run generate.go OUTPUT")
	}
	f, err := os.Create(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	w, err := qwal.NewNativeWriter(f)
	if err != nil {
		panic(err)
	}
	entries := []qwal.Entry{
		{Key: qwal.LogFileKey{Event: qwal.EventOpen, TServerSession: "sort-proof"}},
		{Key: qwal.LogFileKey{Event: qwal.EventDefineTablet, TabletID: 7, Seq: 10,
			Tablet: &qwal.KeyExtent{TableID: "g", EndRow: []byte("m")}}},
		{Key: qwal.LogFileKey{Event: qwal.EventDefineTablet, TabletID: 8, Seq: 20,
			Tablet: &qwal.KeyExtent{TableID: "g", EndRow: []byte("z"), PrevEndRow: []byte("m")}}},
		{Key: qwal.LogFileKey{Event: qwal.EventMutation, TabletID: 7, Seq: 11},
			Value: mutation("pre-compaction", "discard-me", 100)},
		{Key: qwal.LogFileKey{Event: qwal.EventMutation, TabletID: 8, Seq: 21},
			Value: mutation("right", "right-value", 202)},
		{Key: qwal.LogFileKey{Event: qwal.EventCompactionStart, TabletID: 7, Seq: 12,
			Filename: "file:///input/tables/g/t-0001/F0001.rf"}},
		{Key: qwal.LogFileKey{Event: qwal.EventCompactionFinish, TabletID: 7, Seq: 13}},
		{Key: qwal.LogFileKey{Event: qwal.EventMutation, TabletID: 7, Seq: 14},
			Value: mutation("left", "left-value", 101)},
		{Key: qwal.LogFileKey{Event: qwal.EventManyMutations, TabletID: 7, Seq: 15},
			Value: qwal.LogFileValue{Mutations: []qwal.ServerMutation{
				mutation("batch-a", "A", 102).Mutations[0],
				deletedMutation("batch-b", 103),
			}}},
	}
	for _, entry := range entries {
		if err := w.WriteEntry(entry); err != nil {
			panic(err)
		}
	}
	if err := f.Sync(); err != nil {
		panic(err)
	}
	if err := f.Close(); err != nil {
		panic(err)
	}
	fmt.Println("wrote two-tablet native WAL")
}

func mutation(row, value string, systemTime int64) qwal.LogFileValue {
	return qwal.LogFileValue{Mutations: []qwal.ServerMutation{{
		Row: []byte(row), SystemTime: systemTime,
		Updates: []qwal.ColumnUpdate{{ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq"), Value: []byte(value)}},
	}}}
}

func deletedMutation(row string, systemTime int64) qwal.ServerMutation {
	return qwal.ServerMutation{
		Row: []byte(row), SystemTime: systemTime,
		Updates: []qwal.ColumnUpdate{{ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq"),
			HasTimestamp: true, Timestamp: 0, Deleted: true}},
	}
}
